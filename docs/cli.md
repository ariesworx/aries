# Using sfx

`sfx` is the starfix client. People use it as a command line, and agents
run it as an MCP server ([agent guide](agents.md)). This page covers
connecting a repository to a server and every `sfx` command. The ideas
behind the commands are in [How starfix works](concepts.md).

## Connect a repository

A repository connects to a server through a `.starfix.yaml` file in its
root. Commit it: it holds no secrets.

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  host_key: SHA256:…
```

| Field | Holds | Default |
|---|---|---|
| `project` | The project's UUID. It must match the server's `project:` setting | required |
| `server.host` | The server's host name or address | required |
| `server.port` | The SSH port | `22` |
| `server.user` | The account starfixd runs as | `starfix` |
| `server.host_key` | The server's ED25519 host key fingerprint, `SHA256:…` | required |
| `server.iap` | Reach the server through Google Cloud IAP ([below](#servers-without-a-public-ip-google-cloud-iap)) | off |
| `key` | A key file inside the repository, relative to its root: the only key `sfx` then offers ([below](#your-key-is-your-identity)). The file is committed and shared, so most people leave this out; set `STARFIX_KEY_FILE` to name a key of your own | your default identities, then ssh-agent |

### The host key is pinned

`sfx` checks the server's host key on every connection and never trusts a
key on first use. Get the fingerprint from the server's admin, or read it
yourself over a channel you trust:

```sh
ssh-keyscan -t ed25519 starfix.example.com | ssh-keygen -lf -
```

On a cloud server, prefer a value the server publishes through its
provider's API (for example in instance metadata) over a scan across the
network.

### Your key is your identity

The server's admin adds your public key with your principal name
([Principals](server.md#principals)): send them the `.pub` file, never
the file beside it. `sfx` runs its own SSH client, so it does not use
your `~/.ssh/config`. Which keys it offers the server is decided in this
order, and nowhere else:

| | Where the key comes from | What `sfx` offers |
|---|---|---|
| 1 | **`STARFIX_KEY_FILE`**, an environment variable that holds the *path* of a key file, any path (`~/` is your home directory). It is yours alone, since `.starfix.yaml` is committed and shared | That key and no other |
| 2 | **`key:`** in `.starfix.yaml`, which may name only a file inside the repository: a relative path that, with symbolic links resolved, stays under the repository's root. A path outside it is refused, and the fix says to use `STARFIX_KEY_FILE` | That key and no other |
| 3 | **Your default identities**, when neither is set: `~/.ssh/id_rsa`, `id_ecdsa`, `id_ecdsa_sk`, `id_ed25519` and `id_ed25519_sk`, the files `ssh` tries, in its order, and only those that exist | Each one it can use |
| 4 | **ssh-agent** (the OpenSSH agent's named pipe on Windows), after those | The keys it holds that are not already offered, in its order, until five keys are offered in all |

A key named by 1 or 2 is the only key offered, whatever your `~/.ssh` and
ssh-agent hold, as with OpenSSH's `IdentitiesOnly`. The file may be the
private key, or its `.pub`: for a `.pub`, for a private key that is
passphrase-protected, and for a private key that is missing while its
`.pub` is there, ssh-agent signs with that one key, which `sfx` finds by
its public half, and offers no other. This is how an agent is given an
identity of its own
([agent guide](agents.md#give-an-agent-its-own-identity)).

With neither set (3 and 4) `sfx` does what `ssh` does, so that a person
whose key is `~/.ssh/id_ed25519` has nothing to configure:

- A default file that is passphrase-protected is used through ssh-agent
  when the agent holds its key, and is otherwise skipped. `sfx` never
  asks for a passphrase.
- An `_sk` file is a key held on a hardware token. It is used only
  through ssh-agent, found by the `.pub` beside it, and is otherwise
  skipped.
- A certificate beside a key (`id_ed25519-cert.pub`) is not read:
  certificates are not supported.
- sshd counts every key it refuses and closes the connection at
  `MaxAuthTries`, six by default. So `sfx` offers five keys at most. The
  default files come first, and the rest of ssh-agent's keys fill what
  is left; any beyond that are not offered, and a refusal says how many.
- The server takes the first key it knows. With more than one of your
  keys registered, to different principals, that decides who you are;
  name the key you mean with `STARFIX_KEY_FILE`.

The home directory is `HOME` (`USERPROFILE` on Windows), from the
process's environment. Nothing in a repository changes where `sfx` looks
for default identities.

`sfx whoami` shows the result: the principal the server took you for, the
project and server, the fingerprint of the key that signed, where the key
came from (the file, and what named it, or ssh-agent), and any default
file that was skipped, with the reason. `sfx prime`, which every agent
session starts with, says the same in its second line. When the server
accepts no key, the refusal lists each key `sfx` offered, by fingerprint
and file, says what was skipped or left out, and names
`STARFIX_KEY_FILE`.

**`STARFIX_KEY_FILE` holds a path, never a key.** A value that is plainly
a key (more than one line, or the opening of a private key) is refused
without being repeated. `sfx` reads no variable named `STARFIX_KEY`:
earlier instructions for continuous integration put a private key's
contents in one. `sfx` never looks at its value. When it is set and
sign-in fails, the refusal says once that it is not read.

### Environment variables

| Variable | Holds |
|---|---|
| `STARFIX_KEY_FILE` | The path of the key file to sign in with: then the only key offered (above). Never the key itself |
| `SSH_AUTH_SOCK` | ssh-agent's socket |
| `HOME`, or `USERPROFILE` on Windows | Your home directory: `~/` in `STARFIX_KEY_FILE`, and `~/.ssh` for the default identities |
| `STARFIX_SESSION`, `STARFIX_HARNESS`, `TRACEPARENT`, `TRACESTATE` | The session id, the agent harness's name and the caller's trace context ([agent guide](agents.md#sessions)) |

`sfx` reads no other variable to decide who you are.

### Where sfx looks for the file

`sfx` looks for `.starfix.yaml` in the working directory, then in each
parent. It stops at the repository's top level (the first directory that
holds `.git`) and never looks above your home directory. `-C DIR` starts
the search in DIR instead.

On macOS and Linux the file must be yours and writable by no one else.
`chmod go-w .starfix.yaml` fixes a checkout made under a umask of 002.
Windows has no owner check, so there the search boundary is the only
guard.

### Servers without a public IP (Google Cloud IAP)

A server on a Compute Engine instance with no external address can be
reached through Identity-Aware Proxy TCP forwarding. Add an `iap` block;
`server.host` then names the instance:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix-1
  host_key: SHA256:…
  iap:
    project: example-project
    zone: us-central1-a
```

| Field | Holds |
|---|---|
| `iap.project` | The Google Cloud project id |
| `iap.zone` | The instance's zone |
| `iap.instance` | The instance name; defaults to `server.host` |

For each connection, `sfx` runs
`gcloud compute start-iap-tunnel INSTANCE PORT --listen-on-stdin` and
speaks SSH through it, so nobody keeps a tunnel open by hand. The host key
is still pinned and checked. Each developer needs:

- the Google Cloud CLI on `PATH`, signed in with `gcloud auth login`;
- the IAP-secured Tunnel User role on the instance, and permission to look
  the instance up (Compute Viewer on the project is enough).

The instance's firewall must admit IAP's range, `35.235.240.0/20`, on the
SSH port. `sfx setup` warns when `gcloud` is missing.

There is no general proxy setting, on purpose: `.starfix.yaml` is
committed, so a command in it would run whatever a cloned repository says.

## Commands

```text
sfx [-C DIR] [--json] COMMAND [ARGS]
```

`sfx help COMMAND` prints a command's usage. `-C DIR` runs as if started
in DIR. `--json` prints exactly one JSON document, errors included.

### Work on an issue

| Command | Does |
|---|---|
| `start [ID]` | Claim an issue, the top ready one if you give no ID, and show it with its checklist, last handoff, relevant memories and branch name. `--for 4h` sets the lease (default 8h, at most 24h). `--branch` checks the branch out; `--worktree DIR` creates a worktree on it. `--take` takes the claim over from another session of yours |
| `finish ID` | Close your issue and end the claim in one step: `--reason`, a `--handoff` note (with the handoff fields below), and `--discovered TITLE` for each new issue found on the way. `--tick 1,3` and `--waive N=REASON` settle acceptance items first; finish is refused while any item is open. `--epoch N` refuses unless N is still the claim's epoch ([Claims and leases](concepts.md#claims-and-leases)). It sends the paths the issue's work touched, read from git ([Files](concepts.md#files-what-an-issue-touches)) |
| `accept ID N...` | Tick acceptance items. `--undo` unticks them; `--waive REASON` waives them |
| `handoff ID NOTE` | Leave a note for whoever continues. Fields: `--state done\|partial\|blocked`, `--next TEXT`, `--branch B`, `--worktree DIR`, and `--to P` to put it in P's inbox. `--release` ends your claim and unassigns the issue, and takes `--epoch N` like `finish`. It sends the issue's paths like `finish` |
| `away DURATION` | Extend all your claims, in every session, to at least now plus DURATION (up to 7d), for example before you go offline. Only from your own terminal (session `cli`): it is refused when `sfx` runs with another session id, as in an agent's session. It then sends the paths the work on each claim touched, read from git, one issue at a time |

### Find and change issues

| Command | Does |
|---|---|
| `create TITLE` | Create an issue and print its id: `-p N` priority, `-t TYPE`, `--body TEXT` (or `-` for stdin), `--parent ID`, `--account NAME`, `--label L`, and `--paths P,P` for the files the work will touch, relative to the current directory (a directory ends in `/`). Similar closed issues, if any, are listed on stderr |
| `show ID` | Show an issue with its dependencies, claim, account, time held, cost at list price, hours people logged (by whom) and tokens by model ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)), acceptance checklist, latest handoff, likely files (declared first, then the most recent 20) with the issues others hold that overlap them, and similar closed issues. `--compact` is shorter |
| `list` | List issues that are not closed, oldest first, 50 at a time. `-n N` sets the page size (up to 500); `--cursor C` continues, as the `more:` line on stderr shows. Filters: `--status S,S`, `--all` (closed too), `-t TYPE`, `-p N,N`, `--assignee A`, `--parent ID`, `--label L` |
| `ready` | List issues nothing holds back, best first. An issue whose files overlap work another session holds comes last, with `overlaps ID` ([Files](concepts.md#files-what-an-issue-touches)) |
| `blocked` | List issues held back by open blockers |
| `update ID` | Change fields (`--title`, `--body`, `--status`, `-p`, `-t`, `--assignee`, `--parent`, `--account`, …). `--account NAME` sets the account the issue's time and tokens report against; `--account ""` clears it, to inherit the parent's. `--paths P,P` replaces the declared paths and `--paths ""` clears them. `--rev N` applies the change only if the issue is still at revision N. It cannot set `in_progress` (claim with `start`), change the status or assignee of a claimed issue (once the claim's lease has run out, such a change ends the claim and tells its holder), or drop an acceptance item that is still open |
| `close ID`, `reopen ID` | Close (with `--reason`) or reopen an issue. Close is refused while an acceptance item is open; `--force`, for admins only, closes anyway |
| `dep add\|rm FROM TO` | Add or remove a dependency: FROM depends on TO. `--type T` (default `blocks`) |
| `label add\|rm ID LABEL...` | Add or remove labels |
| `comment ID TEXT` | Add a comment (`-` reads stdin) |
| `comments ID`, `history ID` | List all of an issue's comments, or all its changes; `-n N` shows only the newest N |
| `log DURATION ID` | Log your hours on an issue ([Human hours](concepts.md#human-hours)): `1.5h`, `90m` or `1h30m`, from a minute to 24 hours, for today in your time zone or `--on DATE`, at most a year back and a day past today in UTC; `--note TEXT` adds one line. It prints the entry's id. `log --undo ENTRY` undoes one of your entries (an admin may undo anyone's). Bare `log` lists entries, newest day first: `--issue ID`, `--by PRINCIPAL`, 50 or `-n N` (at most 500) |

### See what is happening

| Command | Does |
|---|---|
| `inbox` | List your unread inbox, newest first: 20 items, or `-n N` (at most 100). `--all` includes read items; `--ack ID` (repeatable or comma-separated) or `--ack-all` marks items read |
| `watch` | Print inbox items as they arrive, until ctrl-c (exit 0). With `--json`, one object per line: `{"op":"inbox","item":{…}}`, or `{"op":"resync"}` when it missed items (`sfx inbox` lists them) |
| `tui` | Show the [live board](#the-live-board) until `q` |
| `whoami` | Say who the server takes you for: your principal, the project and server, the session id, and the fingerprint of the key that signed in, with whether it came from a key file or from ssh-agent. `--json` gives the same as one document. Run it when a command acts as someone you did not expect |
| `who` | List the sessions seen in the last 5 minutes (`--since 2h`, up to 7d) and the issues each holds; at most 100 (`-n N`, up to 500), then a count of the rest |
| `digest` | Summarize a window: `--since 24h` (default), `7d`, a date or a time; filter with `--by PRINCIPAL` or `--label L`. Under its header it prints the time issues were held (saying when some tokens were split by time), their cost at list price, the hours logged on the days the window overlaps, the tokens reported by model, and those no issue was held for |
| `cost` | Report the list-price cost of the tokens reported in a window, their amortized cost once an admin has recorded a plan, and the hours logged ([Cost](concepts.md#cost)): `--since 7d`, a date or a time (required), `--until` a date or a time (not included; default now), grouped `--by account` (default), `issue`, `epic`, `person` or `model`. It lists 50 groups, largest first, or `-n N` (at most 500), the rest summed as `(other)`, then the total. `split` marks a group whose tokens came partly from records shared by time with other work, and `unpriced` one with tokens no price covers; a group with no priced token shows `unpriced` for its cost, never $0.00. With plans, each group shows `$X amortized` after its list price; with hours, `1.5h logged` after its tokens (by `model`, people's hours are the group `(human)`) |
| `prime` | Orient a session: your issues in progress, your inbox, the top ready work, memories (pinned, then relevant to your work, then newest) and version notices. `--hook[=AGENT]` is for an agent's session-start hook ([agent guide](agents.md#session-start-hooks)) |

### Memory

Memories are notes kept across sessions, in `project` (the default),
`team` or `user` scope; a `user` memory is yours alone
([Memory](concepts.md#memory)). Every command takes `--scope S`.

| Command | Does |
|---|---|
| `remember KEY TEXT` | Keep a memory and print `KEY (SCOPE) rev N` (`-` reads stdin). `--tag T` (repeatable or comma-separated), `--issue ID` links it to an issue, `--pin` pins it. Without `--rev` the key must be new; `--rev N` replaces the memory at revision N and is refused if it changed since. A replace keeps the tags, issue and pin it is not given; `--tag ''` and `--issue ''` clear them. A memory that looks like it holds a secret is refused |
| `recall [TEXT]` | Search memories: TEXT in the key or body, `--key K` exactly, `--tag T`, `--scope S`. Pinned first, then newest; 20, or `-n N` (at most 500), then a count of the rest |
| `memories` | List the memories you can see, pinned first, then newest; `--scope S`, `-n N` |
| `forget KEY` | Delete a memory. `--rev N` refuses if it changed since revision N |
| `pin KEY`, `unpin KEY` | Pin a memory, so `prime` always shows it, or unpin it |

### Agents and maintenance

| Command | Does |
|---|---|
| `mcp` | Serve the MCP tools for an agent on stdin and stdout ([agent guide](agents.md)) |
| `setup AGENT` | Set an agent up for this repository; see [Set up an agent](agents.md#set-up-an-agent). With `--global`, or for Claude Desktop, `--key-file PATH` also names the key file the agent signs in with, in your own files only ([Give an agent its own identity](agents.md#give-an-agent-its-own-identity)) |
| `usage --hook[=AGENT]` | Send the token counts in the session's transcripts that earlier runs have not sent. Run by Claude Code's Stop, SubagentStop and SessionEnd hooks, which `setup claude-code` installs, with the hook's JSON on stdin; bare `--hook` is Claude Code's, the only agent captured so far. It always exits 0 and sends only counts ([Token usage hooks](agents.md#token-usage-hooks-claude-code)) |
| `admin prices` | List the model prices, in US dollars per million tokens. `admin prices set MODEL --from DATE --input USD --output USD --cache-write USD --cache-write-1h USD --cache-read USD` sets a model's rates from a date (midnight UTC) or an RFC 3339 time, and prints `added`, `replaced` (the same model and `--from`) or `unchanged`. Every rate must be given, 0 where the model charges nothing, with at most six decimals. Setting is for admins ([Prices](server.md#prices)) |
| `admin plans` | List the subscription plans, in US dollars a seat a month, with each row's monthly total. `admin plans set NAME --from YYYY-MM --fee USD --seats N --principal P...` sets a plan's terms from a month until a later `--from` of the same name, the principals being those whose usage it pays for (repeatable or comma-separated), and prints `added`, `replaced` (the same name and `--from`) or `unchanged`. `--fee 0 --seats 0` ends a plan. Setting is for admins ([Plans](server.md#plans)) |
| `admin project` | On a server of [many projects](server.md#serving-many-projects). `admin project add NAME [--prefix P] [--account A]` creates a project, in a database the server takes from the names its operator granted, and prints its UUID and the `.starfix.yaml` to commit to its repository: this repository's server with the new project, never your key path. NAME is a code name; the prefix of its issue ids defaults to NAME and its account to `internal`. Run again after a failure, it finishes the same project. `admin project list` lists every project for a server admin, and for anyone else the active projects they belong to, with their role; `admin project archive PROJECT` stops serving a project, closes every connection to it and releases every claim in it, keeping its data; `admin project activate PROJECT` serves it again. PROJECT is a UUID or a name. Adding, archiving and activating are for server admins ([Managing access](server.md#managing-access)) |
| `admin principal` | `admin principal add NAME [--key FILE]... [--project P]` adds a principal as a member of this repository's project, or of `--project` (a UUID or a name); `--no-project`, for server admins, adds it to none. Each `--key` is a public key file to add with it, as `admin key add` takes one; the principal and its keys are added together or not at all. `admin principal list` lists them all, for server admins; `--created-by NAME` lists those NAME created, which a project admin may also ask, and is shown the ones in their projects. `admin principal disable NAME` stops a principal's access everywhere, closes its connections and releases its claims, and says how many; a disabled principal is refused even if it is a server admin, and the last enabled server admin cannot be disabled. Disabling undoes nothing the principal did: when NAME added keys that are still in the store, created principals or added memberships, a warning on stderr counts them and names the lists that show them (`key list --added-by`, `principal list --created-by`, `member list --added-by`), and `--json` carries the counts as `keys_added`, `principals_created` and `members_added`. `admin principal enable NAME` restores it. A project admin may add a principal to a project they administer, and disable or enable one that belongs only to projects they administer |
| `admin key` | A principal's public SSH keys, on a server whose sshd reads the keys file starfixd writes ([Keys from the store](server.md#keys-from-the-store)). `admin key add NAME FILE` adds the key in FILE, a `.pub` file such as `~/.ssh/id_ed25519.pub`, or `-` to read it from standard input, and prints its fingerprint. `sfx` reads the file here and sends the public key alone: a file that holds a private key, more than one line, or anything that is not one public key is refused before anything is sent. A key the principal already has changes nothing; one another principal has is refused, and so is one that the server's hand-maintained `authorized_keys` gives to another principal. `admin key list NAME` lists a principal's keys by fingerprint, type and comment: for server admins, an admin of any active project NAME belongs to (they may see the keys even where they may not change them), and NAME themselves. `admin key list` marks a key that is also a line of the server's hand-maintained `authorized_keys` file, and says how many of NAME's open connections came in by that file. `admin key list --added-by NAME` lists instead the keys NAME added, whoever each belongs to, by fingerprint (`-n N`, `--after FINGERPRINT`): everything for a server admin, and for a project admin the keys of their projects' members. `admin key remove FINGERPRINT` removes the key with that `SHA256:…` fingerprint and closes the connections it had signed in; it warns on stderr when the key is also a line of the server's `authorized_keys`: its connections were closed, and sshd, which reads that file after the server's own, still accepts the key by that line until the line is deleted on the server. Any of these commands that changes which keys sign in also warns when the server recorded the change and could not write its keys file, so that sshd may still be reading the old one. Adding is for server admins and for a project admin with reach over the principal, and never for your own account: another admin adds your keys. Removing is for the same admins, and for yourself: you may remove any key of your own except the one this connection signed in with, which is refused by fingerprint (`sfx whoami` shows it; `admin key list YOU` shows the rest, with who added each). On a connection that came in by the server's `authorized_keys`, which names no key, the removal goes ahead with a warning that it could not be checked |
| `admin member` | `admin member add NAME [--project P] [--role admin] [--accept-keys]` makes a principal a member of the project, or changes its role (`member`, the default, or `admin`), and prints `added`, the new role, or that nothing changed. A key signs in to every project its principal is in, so when NAME has any key and is not yet a member, nothing is added at first: `sfx` lists every key NAME has, with who added it, when, and whether that was an admin of this project, and on a terminal asks before it adds. Without a terminal it exits 1 with a fix naming `--accept-keys`, which adds without asking; with `--json` the one document holds the refusal under `error` and the keys under `member`. Someone with no key is added at once, and a change of role asks nothing. `admin member list [--project P]` lists the project's members; with `--added-by NAME` it lists the memberships NAME added, in every project you may list the members of, or in `--project`, by project and principal (`--after PROJECT/PRINCIPAL` continues). `admin member remove NAME [--project P]` ends a membership, closes that principal's connections to the project and releases the claims it held there, saying how many; it warns on stderr when that leaves the principal within reach of project admins who could not manage it before. For the project's admins and server admins. Each `admin` list of projects, principals or members is by name, 200 rows or `-n N` (at most 1000), then a count of the rest, which `--after NAME` lists |
| `admin status` | What the server knows of itself and its projects now ([Watching a server](server.md#watching-a-server)). A server admin sees the server (version and protocol range, uptime, open stores and connections beside their limits, the keys file it writes for sshd and whether its last write failed, starfixd's own memory and goroutines, projects and principals by state) and a row for every project; a project admin sees the rows of the active projects they administer; anyone else is refused. A row gives the project's state, whether its store is open, its connections and how many of them came in by the hand-maintained `authorized_keys`, the sessions seen in five minutes, the writes of the last minute, and the median and 99th percentile latency of its latest 256 writes. `-n N` and `--after NAME` page the projects. It changes nothing, and reports nothing of Dolt's memory or disk |
| `upgrade` | Replace `sfx` with the latest release after checking its signature and checksum. `--check` reports and changes nothing; `--rollback` restores the binary the last upgrade replaced. It never runs on its own |
| `version` | Print the version and protocol |

## The live board

`sfx tui` fills the terminal with a board that keeps itself current:

- **Ready**: the issues nothing holds back, best first, up to 100. An
  issue whose files overlap work another session holds is marked
  `[overlaps ID]` ([Files](concepts.md#files-what-an-issue-touches)).
- **Held**: each issue under a live claim, with its holder as
  `principal/session` and how long they have held it, on the server's
  clock. `[lapsed]` marks a lease that ran out before the reaper freed
  the issue.
- **Blocked**: the issues held back by open blockers, `[by ID +N]`.
- **Events**: every change to an issue since the board opened, newest at
  the bottom, with its time, who made it and what it was.

From 60 columns the board is two columns, ready and blocked beside held,
with the events below; narrower, it is one.

| Key | Does |
|---|---|
| `j`, `k`, arrows | Move the selection |
| `PgUp`, `PgDn`, `g`, `G` | Move a page, or to the top or bottom |
| `Enter`, right arrow | Open the selected issue: its fields, claim, dependencies, files, checklist, handoff and body |
| `Esc`, left arrow, `Backspace` | Go back |
| `r` | Read everything again |
| `?` | Show the keys and marks |
| `q`, `Ctrl-C` | Quit |

The board rides the same SSH connection as every other command, so the
server opens no port for it. It asks the server to push every change to
an issue, and reads the lists again only when a change can alter them,
once per burst; an open issue is read again when a change names it. It
never polls: the only timer redraws relative times each second. If the
connection drops, the header says `reconnecting`, and the board redials
and reads everything afresh. Each redial waits a random time up to a
limit that starts at 1 s and doubles to 30 s; the limit starts over only
once a new connection has read the lists. The board is read-only.

`sfx tui` needs a terminal on standard input and output, and one that
does not say `TERM=dumb`. Anywhere else it refuses and points at
`sfx ready`, `sfx blocked` and `sfx list`. It
honors [`NO_COLOR`](https://no-color.org): set, the board draws without
color and marks the selection with `>` alone. Like all output, text from
the server is escaped (below).

## Exit codes

| Code | Means |
|---|---|
| 0 | Success |
| 1 | Failure. stderr ends with a `fix:` line saying what to do next |
| 2 | Usage error: an unknown flag or a missing argument |
| 3 | The server refused this client's protocol version; the `fix:` line says which side to upgrade |

A refusal exits 1 like any other failure, and `--json` gives its code.
When another principal holds an issue, a change to it is refused with
`forbidden`, as is a change reserved for admins, and a connection to a
project you are not a member of. `start` is refused with
`conflict` instead, and names the next ready issue. A request past one of
the server's limits is refused with `invalid` or `busy`
([Limits](server.md#limits)).

## Text from others is shown safely

Issue text comes from other people and their agents. `sfx` prints any
control or bidirectional character as a visible escape (`\x1b`,
`\u202e`), so text cannot clear your terminal or disguise itself as
`sfx`'s own output. [Security](security-model.md#untrusted-text) has the full
rules.
