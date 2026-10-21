# Using `aries`

`aries` is the Aries client. People use it as a command line, and agents
run it as an MCP server ([agent guide](agents.md)). This page covers
connecting a repository to a server and every `aries` command. The ideas
behind the commands are in [How Aries works](concepts.md).

## Connect a repository

A repository connects to a server through a `.aries.yaml` file in its
root. Commit it: it holds no secrets.

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  url: https://aries.example.com
```

A server reached over HTTPS, the transport a fresh server serves, is
named by its URL. One that serves SSH, as servers did before HTTPS, is
named by its host and the fingerprint of its host key; a server that
serves both may be named both ways:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  url: https://aries.example.com   # HTTPS: what aries uses
  host: aries.example.com          # SSH: with ARIES_TRANSPORT=ssh
  host_key: SHA256:…
```

| Field | Holds | Default |
|---|---|---|
| `project` | The project's UUID. It must match the server's `project:` setting | required |
| `server.url` | The server's `https://` URL, the one its operator gives ([The HTTPS listener](server.md#the-https-listener)). `aries` uses HTTPS when it is set, unless `ARIES_TRANSPORT=ssh` | required, or `host` |
| `server.ca` | A certificate authority for a server whose certificate the system's store does not trust, a company's or a site's own: its certificate in PEM (a `\|` block), or a PEM file inside the repository, by a path relative to its root. It is added to the system's roots for this server, never in their place; a CA's certificate is public, so it may be committed | the system's roots |
| `server.host` | The server's host name or address, for SSH | required without `url` |
| `server.port` | The SSH port | `22` |
| `server.user` | The account ariesd runs as | `aries` |
| `server.host_key` | The server's ED25519 host key fingerprint, `SHA256:…` | required with `host`, or `host_keys` |
| `server.host_keys` | A list of such fingerprints, in place of `host_key`, while the server's host key is rotated ([below](#the-host-key-is-pinned)) | none |
| `server.iap` | Reach the server through Google Cloud IAP ([below](#servers-without-a-public-ip-google-cloud-iap)) | off |
| `key` | A key file inside the repository, relative to its root: the only key `aries` then offers ([below](#your-key-is-your-identity)). The file is committed and shared, so most people leave this out; set `ARIES_KEY_FILE` to name a key of your own | your default identities, then ssh-agent |

Everyone on the team reads the same committed file, with whatever
version of `aries` they have, so a key that this `aries` does not know,
such as one a newer release reads, is ignored rather than refused. A
command that connects says so once on standard error, naming each such
key (`aries: warning: .aries.yaml has keys this aries does not know,
ignored: …`), and `prime` gives the same line to an agent as a notice.
A key it knows is still checked, and a bad value is still refused.

### HTTPS or SSH

`aries` and `aries mcp` reach the server over HTTPS when `server.url`
is set, and over SSH when it is not. `ARIES_TRANSPORT=ssh` chooses SSH
where both are named, for a server reached through IAP, say;
`ARIES_TRANSPORT=https` insists on the URL. A transport the file does
not name the server for is refused, with what to set. Over HTTPS
`aries` opens one WebSocket to the server and speaks the same frames as
over SSH. With a sign-in kept on the machine ([Sign in](#sign-in)), it
presents the sign-in's token with a proof by its key; without one, it
proves it holds one of the keys below by signing a challenge of the
server's and the server's URL ([How aries signs in over
HTTPS](server.md#how-aries-signs-in-over-https)): the same keys, chosen
the same way, as over SSH. `aries whoami` says which transport, and
which sign-in or key.

What fails over HTTPS, and what is kept to send later
([When the server cannot be reached](#when-the-server-cannot-be-reached)):

| What fails | `aries` says | A write is queued |
|---|---|---|
| The name does not resolve, nothing answers, the connection is reset, TLS does not finish in time, or a proxy answers 502, 503 or 504 | `cannot reach HOST:PORT`, or `HOST:PORT answered 503 …` | Yes |
| The server's certificate does not verify | `the TLS certificate of HOST:PORT does not verify`, with a fix naming `server.ca` | No: it is a setting to correct, or something between you and the server |
| The server knows none of your keys | `HOST:PORT accepted no key: aries offered …`, as over SSH, with `aries login` named as the other way in | No |
| Your sign-in ended, was ended, or its key on this machine is not its own | `your sign-in ended at …`, or why the server refused it, with a fix naming `aries login` | No |
| A proxy refuses the WebSocket upgrade | `HOST:PORT answered the WebSocket upgrade with 400 …`, with a fix naming the proxy | No |
| The server refuses you (disabled, not a member) | Its refusal, as over SSH | No |

A server with SSH off refuses every key over SSH, since sshd has none
for the account; when the file also has `server.url`, the refusal says
to unset `ARIES_TRANSPORT`.

### Sign in

Where the server signs people in at the company's identity provider
([Sign-in](server.md#sign-in)), `aries login` does it, over HTTPS, and
every `aries` and `aries mcp` you run that reaches that server uses the
sign-in, whatever the repository, until it ends:

```console
$ aries login
aries: opened your browser to sign in at corp; finish there (waiting up to 10 minutes)
signed in as alice at corp, until 2026-10-17 09:00 UTC (sign-in g-7f3a2c9d1e4b5a60)
$ aries whoami
principal  alice
…
signed in  at corp, until 2026-10-17 09:00 UTC (sign-in g-7f3a2c9d1e4b5a60, scopes cli mcp); `aries logout` ends it
$ aries logout
signed out of https://aries.example.com (sign-in g-7f3a2c9d1e4b5a60)
```

- **The browser.** `aries login` listens on a port of `127.0.0.1` and
  opens your browser at the server, which sends it on to the provider;
  you sign in there, and the browser comes back to `aries`, which says
  how it went. The provider asks you to sign in again each time
  (`prompt=login`), even with a session open: that is what keeps a
  day's sign-in from renewing with nobody there.
- **No browser here** (over SSH, in a container), or **`--url`**:
  `aries login` prints the URL. Open it in a browser on any machine and
  sign in; the browser then fails to open a page at
  `http://127.0.0.1:…`, which is expected. Copy that whole address from
  its address bar and paste it where `aries login` waits. If the
  browser is on this machine, or the port is forwarded, it finishes by
  itself.
- **`--provider NAME`** chooses where the server has several; with one,
  it is that.
- **Who you are** is the principal an admin invited your address for at
  that provider ([Identities and
  invitations](server.md#identities-and-invitations)); nobody else can
  sign in. The first sign-in binds the invitation to your account at the
  provider, and from then on that account is you, whatever its address
  becomes.
- **What is kept.** The token the server issued and a key made for it,
  in your data directory (`ARIES_DATA_DIR`, or the system's: on Linux
  `~/.local/share/aries`, on macOS `~/Library/Application Support/aries`,
  on Windows `%LocalAppData%\aries`), under `signin/`, each file 0600
  in a 0700 directory; a sign-in in a directory others can read is
  refused, never used. The token is useless without the key: each
  connection proves it holds it. Neither is ever printed.
- **A day.** A sign-in lasts the server's `token_lifetime`, 24 hours by
  default, and nothing renews it: when it ends, its connections close
  and the next command says to run `aries login`. A new `aries login`
  replaces this machine's last sign-in, and ends it at the server.
- **Each sign-in is announced** in your inbox in every project you are
  a member of, as a `signin.granted` item from `ariesd` with the
  provider, the machine and when it ends
  ([The inbox](concepts.md#the-inbox)). One you did not make is someone
  else working as you: end it with the `aries admin signin end ID` the
  item names.
- **`aries logout`** ends the sign-in at the server, which closes its
  connections at once, and removes it from this machine. With the
  server out of reach, it removes it here and says that an admin can
  end it (`aries admin signin end ID`).
- **Without a sign-in**, `aries` proves a key over HTTPS as before
  ([Your key is your identity](#your-key-is-your-identity)), and a
  refusal of every key says that `aries login` is the other way in.
- **Agents.** `aries mcp` uses the sign-in too; when it ends, the agent
  is told to ask you to run `aries login`, and never to run it itself.
- **A CI job or a cloud workload** signs in as its service principal
  with `aries login --workload KIND` (or `ARIES_WORKLOAD=KIND`), where
  KIND is `github-actions`, `google` or `azure`, and the server has a
  workload provider of that kind ([Service
  principals](server.md#service-principals)): `aries` makes a key, asks
  the platform for the workload's token for the server's audience,
  bound to that key where the platform allows, and exchanges it for a
  sign-in of scope `ci` as the service a trust rule names, kept as
  yours would be. `--provider NAME` chooses among providers of one
  kind. GitHub Actions needs the job's `permissions: id-token: write`,
  and `ARIES_DATA_DIR` in `${{ runner.temp }}`, which `aries` asks for
  on a runner ([CI gates](ci.md#github-actions)); Google Cloud reads the
  metadata server (`GCE_METADATA_HOST` names another); Azure, a managed
  identity, from the instance metadata service or `IDENTITY_ENDPOINT`
  and `IDENTITY_HEADER`. It prints `signed in as ci-example by trust
  rule ci-example-ci at github-actions, until …`; `aries whoami` names
  the rule, and `aries logout` ends it.

### The host key is pinned

`aries` checks the server's host key on every connection and never trusts a
key on first use. Get the fingerprint from the server's admin, or read it
yourself over a channel you trust:

```sh
ssh-keyscan -t ed25519 aries.example.com | ssh-keygen -lf -
```

On a cloud server, prefer a value the server publishes through its
provider's API (for example in instance metadata) over a scan across the
network.

While the server's admin rotates its host key, the file pins both keys,
and `aries` connects when the server presents either one:

```yaml
server:
  host: aries.example.com
  host_keys:
    - SHA256:…   # the key the server has now
    - SHA256:…   # the key it will have
```

Set `host_key` or `host_keys`, not both: a file with both is refused, as
is a list that names a key twice. Once the rotation is done, the old
key is removed and the file goes back to `host_key` ([Rotating the host
key](server.md#rotating-the-host-key)). Only an `aries` that knows
`host_keys` reads a file that has it; an older one refuses the whole
file, so upgrade `aries` before the list is committed.

A server that presents none of the pinned keys is refused, with the key
it presented and the ones pinned:

```text
aries: host key mismatch for aries.example.com: the server presented SHA256:…, but .aries.yaml pins SHA256:… and SHA256:…
fix: if the server's host key was changed, confirm its new key with the admin out of band, then add it to server.host_keys; otherwise do not connect
```

### Your key is your identity

The server's admin adds your public key with your principal name
([Principals](server.md#principals)): send them the `.pub` file, never
the file beside it. `aries` runs its own SSH client, so it does not use
your `~/.ssh/config`. Which keys it offers the server is decided in this
order, and nowhere else:

| | Where the key comes from | What `aries` offers |
|---|---|---|
| 1 | **`ARIES_KEY_FILE`**, an environment variable that holds the *path* of a key file, any path (`~/` is your home directory). It is yours alone, since `.aries.yaml` is committed and shared | That key and no other |
| 2 | **`key:`** in `.aries.yaml`, which may name only a file inside the repository: a relative path that, with symbolic links resolved, stays under the repository's root. A path outside it is refused, and the fix says to use `ARIES_KEY_FILE` | That key and no other |
| 3 | **Your default identities**, when neither is set: `~/.ssh/id_rsa`, `id_ecdsa`, `id_ecdsa_sk`, `id_ed25519` and `id_ed25519_sk`, the files `ssh` tries, in its order, and only those that exist | Each one it can use |
| 4 | **ssh-agent** (the OpenSSH agent's named pipe on Windows), after those | The keys it holds that are not already offered, in its order, until five keys are offered in all |

A key named by 1 or 2 is the only key offered, whatever your `~/.ssh` and
ssh-agent hold, as with OpenSSH's `IdentitiesOnly`. The file may be the
private key, or its `.pub`: for a `.pub`, for a private key that is
passphrase-protected, and for a private key that is missing while its
`.pub` is there, ssh-agent signs with that one key, which `aries` finds by
its public half, and offers no other. This is how an agent is given an
identity of its own
([agent guide](agents.md#give-an-agent-its-own-identity)). A private key
that other users can read (any group or other bit in its mode) is
refused, with the fix `chmod 600`, as `ssh` refuses it; a `.pub` is not
checked. Windows modes are not checked.

With neither set (3 and 4) `aries` does what `ssh` does, so that a person
whose key is `~/.ssh/id_ed25519` has nothing to configure:

- A default file that other users can read is skipped, saying so and
  naming `chmod 600`, as `ssh` skips it.
- A default file that is passphrase-protected is used through ssh-agent
  when the agent holds its key, and is otherwise skipped. `aries` never
  asks for a passphrase.
- An `_sk` file is a key held on a hardware token. It is used only
  through ssh-agent, found by the `.pub` beside it, and is otherwise
  skipped.
- A certificate beside a key (`id_ed25519-cert.pub`) is not read:
  certificates are not supported.
- sshd counts every key it refuses and closes the connection at
  `MaxAuthTries`, six by default. So `aries` offers five keys at most. The
  default files come first, and the rest of ssh-agent's keys fill what
  is left; any beyond that are not offered, and a refusal says how many.
- The server takes the first key it knows. With more than one of your
  keys registered, to different principals, that decides who you are;
  name the key you mean with `ARIES_KEY_FILE`.

The home directory is `HOME` (`USERPROFILE` on Windows), from the
process's environment. Nothing in a repository changes where `aries` looks
for default identities.

`aries whoami` shows the result: the principal the server took you for, the
project and server, the fingerprint of the key that signed, where the key
came from (the file, and what named it, or ssh-agent), and any default
file that was skipped, with the reason. `aries prime`, which every agent
session starts with, says the same in its second line. When the server
accepts no key, the refusal lists each key `aries` offered, by fingerprint
and file, says what was skipped or left out, and names
`ARIES_KEY_FILE`.

**`ARIES_KEY_FILE` holds a path, never a key.** A value that is plainly
a key (more than one line, or the opening of a private key) is refused
without being repeated. `aries` reads no variable named `ARIES_KEY`:
earlier instructions for continuous integration put a private key's
contents in one. `aries` never looks at its value. When it is set and
sign-in fails, the refusal says once that it is not read.

### Environment variables

| Variable | Holds |
|---|---|
| `ARIES_KEY_FILE` | The path of the key file to sign in with: then the only key offered (above). Never the key itself |
| `ARIES_TRANSPORT` | `ssh` or `https`: the transport, where `.aries.yaml` names the server both ways ([HTTPS or SSH](#https-or-ssh)). Unset, `server.url` wins |
| `SSH_AUTH_SOCK` | ssh-agent's socket |
| `HOME`, or `USERPROFILE` on Windows | Your home directory: `~/` in `ARIES_KEY_FILE`, and `~/.ssh` for the default identities |
| `ARIES_SESSION`, `ARIES_HARNESS`, `TRACEPARENT`, `TRACESTATE` | The session id, the agent harness's name and the caller's trace context ([agent guide](agents.md#sessions), [Trace context](agents.md#trace-context)) |
| `CI` | Set to `true`, as GitHub Actions and GitLab CI set it, and no harness names a session: `aries` runs in the session `ci`, a CI job's, rather than `cli` ([CI gates](ci.md#who-may-report)) |
| `GITHUB_HEAD_REF`, `GITHUB_REF_NAME` with `GITHUB_REF_TYPE`, `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_BRANCH` | The branch a CI job runs on, for `aries gate pass\|fail --from-branch`, in that order; git's branch when none is set |
| `ARIES_LOCK_EPOCH` | Set by `aries lock run` for the command it runs: the epoch of the lock's hold, for a resource that checks it ([Locks](#locks)). `aries` itself never reads it |
| `ARIES_NO_QUEUE` | Set to anything but empty, `0` or `false`: a write that cannot reach the server fails and is not queued, as with `--no-queue` ([When the server cannot be reached](#when-the-server-cannot-be-reached)) |
| `ARIES_DATA_DIR` | The absolute path of the directory `aries` keeps its outbox, cache and sign-in in, in place of your data directory ([Where it is kept](#where-it-is-kept)) |
| `ARIES_WORKLOAD`; `ACTIONS_ID_TOKEN_REQUEST_URL`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, `RUNNER_TEMP`; `GCE_METADATA_HOST`; `IDENTITY_ENDPOINT`, `IDENTITY_HEADER` | For `aries login --workload` alone ([Sign in](#sign-in)): the kind, as `--workload` takes it; where GitHub Actions gives the job its OIDC token, and the runner's temporary directory, in which `ARIES_DATA_DIR` must be set; another Google Cloud metadata server; and an Azure managed identity's endpoint where App Service, Functions or Container Apps set one. Each is the platform's to set |
| `ARIES_RELEASES` | Where `aries upgrade` reads releases from, in place of the default: a mirror's `https://` URL, or the absolute path of a directory ([Where releases are read from](install.md#where-releases-are-read-from)) |

`aries` reads no other variable to decide who you are.

### Where aries looks for the file

`aries` looks for `.aries.yaml` in the working directory, then in each
parent. It stops at the repository's top level (the first directory that
holds `.git`) and never looks above your home directory. `-C DIR` starts
the search in DIR instead.

A linked worktree (`git worktree add`), where agents' parallel sessions
often run, is its own top level. When the search reaches one without
finding the file, `aries` reads the one at the top of the repository's
main worktree, and only there, so a `.aries.yaml` you keep untracked in
your main checkout serves every worktree. A file in the worktree itself
still wins. A key or CA path in the file is relative to the directory
that holds it, the main worktree, while `aries setup` writes its files
in the worktree you ran it in. A bare repository has no main worktree,
so its worktrees need a file of their own.

On macOS and Linux the file must be yours and writable by no one else.
`chmod go-w .aries.yaml` fixes a checkout made under a umask of 002.
Windows has no owner check, so there the search boundary is the only
guard.

### Servers without a public IP (Google Cloud IAP)

A server on a Compute Engine instance with no external address can be
reached through Identity-Aware Proxy TCP forwarding. Add an `iap` block;
`server.host` then names the instance:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: aries-1
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

For each connection, `aries` runs
`gcloud compute start-iap-tunnel INSTANCE PORT --listen-on-stdin` and
speaks SSH through it, so nobody keeps a tunnel open by hand. The host key
is still pinned and checked. Each developer needs:

- the Google Cloud CLI on `PATH`, signed in with `gcloud auth login`;
- the IAP-secured Tunnel User role on the instance, and permission to look
  the instance up (Compute Viewer on the project is enough).

The instance's firewall must admit IAP's range, `35.235.240.0/20`, on the
SSH port. `aries setup` warns when `gcloud` is missing.

There is no general proxy setting, on purpose: `.aries.yaml` is
committed, so a command in it would run whatever a cloned repository says.

## Commands

```text
aries [-C DIR] [--json] [--no-queue] COMMAND [ARGS]
```

`aries help` lists the commands by task: issues, coordination, memory,
cost and usage, gates, offline and cache, setup, and admin, with a line
for each. `aries help COMMAND`, or `aries COMMAND --help` (`-h`), prints
a command's usage, its flags with their defaults, and two or three
examples; `aries help admin member`, or `aries gate add --help`, those
of one subcommand. Help asked for exits 0; a bare `aries` prints the
list and exits 2, as a mistake on the command line does
([Exit codes](#exit-codes)). Help is wrapped to 80 columns:

```text
$ aries help start
usage: aries start [ID] [--for DURATION] [--take]
               [--new-branch | --worktree DIR]

claim an issue (the top ready one without ID) and show it

flags:
  --for DURATION  how long the claim lasts (1m to 24h); `aries away` extends it
                  (default 8h)
  --new-branch    create or switch to the issue's branch here
  --take          take it over from another session of yours that holds it
  --worktree DIR  create a worktree at DIR on the issue's branch

examples:
  # Claim the top ready issue
  aries start

  # Claim one and check its branch out here
  aries start ar-a1b2c3d4 --new-branch

  # Claim it for 4 hours, in a worktree of its own
  aries start ar-a1b2c3d4 --for 4h --worktree ../login-fix
```

`-C DIR` runs as if started
in DIR. `--json` prints exactly one JSON document, errors included.
`--no-queue` makes a write that cannot reach the server fail instead of
waiting in the outbox
([When the server cannot be reached](#when-the-server-cannot-be-reached)).

### Work on an issue

| Command | Does |
|---|---|
| `start [ID]` | Claim an issue, the top ready one if you give no ID, and show it with its checklist, last handoff, relevant memories and branch name. `--for 4h` sets the lease (default 8h, at most 24h). `--new-branch` checks the branch out, making it if it is new; `--worktree DIR` creates a worktree on it. `--take` takes the claim over from another session of yours |
| `finish ID` | Close your issue and end the claim in one step: `--reason`, a `--handoff` note (with the handoff fields below), and `--discovered TITLE` for each new issue found on the way. `--tick 1,3` and `--waive N=REASON` settle acceptance items first; finish is refused while any item is open. `--epoch N` refuses unless N is still the claim's epoch ([Claims and leases](concepts.md#claims-and-leases)). It sends the paths the issue's work touched, read from git ([Files](concepts.md#files-what-an-issue-touches)) |
| `accept ID N...` | Tick acceptance items. `--undo` unticks them; `--waive REASON` waives them |
| `handoff ID NOTE` | Leave a note for whoever continues. Fields: `--state done\|partial\|blocked`, `--next TEXT`, `--branch B`, `--worktree DIR`, and `--to P` to put it in P's inbox. `--release` ends your claim and unassigns the issue, and takes `--epoch N` like `finish`. It sends the issue's paths like `finish` |
| `away DURATION` | Extend all your claims, in every session, to at least now plus DURATION, for example before you go offline or on leave: a claim with less than DURATION left is extended, whatever it has left, and none is shortened. DURATION is from 1m to the server's `away_max`, 7d unless its admin set another ([Limits](server.md#limits)). Each claim it extends is on the record, as a `claim.away` event in the issue's history, and the reaper, and a clock that jumps forward on the server, honor it as any lease. There is no `--back`: when you are back sooner, `finish` or `handoff --release` what you are done with, and the rest holds until the time you gave. Only from your own terminal (session `cli`): it is refused when `aries` runs with another session id, as in an agent's session, and no MCP tool does it. It then sends the paths the work on each claim touched, read from git, one issue at a time |

The branch `start` names is `TYPE/ID-SLUG`: the issue's id, then its
title in lowercase letters and digits joined by hyphens, at most 40
characters, as in `fix/ar-a1b2c3d4-fix-the-login-redirect`. TYPE comes
from the issue's type:

| Issue type | Branch prefix |
|---|---|
| `bug` | `fix/` |
| `task`, `chore` | `maintenance/` |
| `feature`, `epic`, `gate` | `feature/` |

The map is the same in every repository; a setting for it may come in a
1.x release. A [CI job](ci.md) finds the issue in such a branch's name,
which works for a project whose prefix has no hyphen of its own.

### Find and change issues

| Command | Does |
|---|---|
| `create TITLE` | Create an issue and print its id: `-p N` priority, `-t TYPE`, `--body TEXT` (or `-` for stdin), `--parent ID`, `--account NAME`, `--label L`, `--paths P,P` for the files the work will touch, relative to the current directory (a directory ends in `/`), and `--defer-until DATE` and `--due DATE` ([Dates](concepts.md#dates): a date, `2026-11-02`, or an RFC 3339 time). Similar closed issues, if any, are listed on stderr, and so is how many acceptance items the project's [workflow](concepts.md#the-workflow) added |
| `show ID` | Show an issue with its dates, the gates that hold it (with who may pass each), its CI checks with their last reports and, once closed, whether it landed ([CI gates](concepts.md#ci-gates)), dependencies, claim, open [conflicts](#conflicts), account, time held, cost at list price, hours people logged (by whom) and tokens by model ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)), acceptance checklist, latest handoff, likely files (declared first, then the most recent 20) with the issues others hold that overlap them, and similar closed issues. `--compact` is shorter |
| `list` | List issues that are not closed, oldest first, 50 at a time, with each one's dates after its title. `-n N` sets the page size (up to 500); `--cursor C` continues, as the `more:` line on stderr shows. Filters: `--status S,S`, `--all` (closed too), `-t TYPE`, `-p N,N`, `--assignee A`, `--parent ID`, `--label L` |
| `ready` | List issues nothing holds back, best first. An issue with a child that is not closed is left out ([Parents and their children](concepts.md#parents-and-their-children)). An issue whose files overlap work another session holds comes last, with `overlaps ID` ([Files](concepts.md#files-what-an-issue-touches)) |
| `blocked` | List issues held back by open blockers (`blocked by ID`, with `via ID` when a parent's blockers hold it), or waiting on their own open children (`waiting on N open children`), then each [gate](concepts.md#gates) among them with who may pass it. `--json` gives `children`, the count of open children, when there are any |
| `update ID` | Change fields (`--title`, `--body`, `--status`, `-p`, `-t`, `--assignee`, `--parent`, `--account`, …). `--account NAME` sets the account the issue's time and tokens report against; `--account ""` clears it, to inherit the parent's. `--paths P,P` replaces the declared paths and `--paths ""` clears them. `--defer-until DATE` and `--due DATE` set the dates, and `""` clears one. `--rev N` applies the change only if the issue is still at revision N. It cannot set `in_progress` (claim with `start`), change the status or assignee of a claimed issue (once the claim's lease has run out, such a change ends the claim and tells its holder), or drop an acceptance item that is still open |
| `conflicts [ISSUE]` | List the open [conflicts](#conflicts), of one issue or all, oldest first: each with its base, mine and theirs on a line each (cut at 500 bytes), the server's advice, and any proposal; 20, or `-n N` (at most 100), and `--after N` for the next page. `conflicts N` shows conflict N whole, open or resolved, each side on lines of its own |
| `resolve N` | Resolve conflict N: `--mine` takes the replayed update's value, `--theirs` keeps the server's, `--text TEXT` (or `-` for stdin) sets new text, and `--accept` does what its proposal says. It changes the issue as `update` does, so it is refused on an issue another principal holds, unless you are an admin. It is never queued |
| `close ID`, `reopen ID` | Close (with `--reason`) or reopen an issue. Close is refused while an acceptance item is open; `--force`, for admins only, closes anyway |
| `dep add\|remove FROM TO` | Add or remove a dependency: FROM depends on TO. `--type T` (default `blocks`). `rm` is accepted for `remove`. A parent already waits on its children, so an edge from an issue to one under it, or from a child to its parent, is refused as a cycle ([Parents and their children](concepts.md#parents-and-their-children)). Removing an edge that a gate holds FROM through is refused unless you could pass the gate |
| `gate add --blocks ID` | Hold ID back behind a new human [gate](concepts.md#gates) and print the gate's id: `--approver P` (repeatable) names who may pass it, none leaves it to the project's admins; `--title T` (default `Approve ID`). The approvers get a `gate.pending` inbox item |
| `gate add --kind ci --check NAME --issue ID --approver P` | Add a [ci gate](concepts.md#ci-gates): it waits for CI principal P (repeatable) to report check NAME passed on ID, the issue whose branch the check runs on. `--blocks ID` holds an issue back behind it; without it the gate only records the check, as for `landed`. An issue has one ci gate for each check |
| `gate pass ID`, `gate rearm ID` | Pass a gate, which lets what it held become ready and tells its assignees, or reopen a passed one so that it holds again; `--reason TEXT`. Only an approver (or, when the gate names none, an admin other than its maker) passes a human gate, and only from your own terminal: run from an agent's session it is refused |
| `gate pass\|fail --check NAME` | Report a ci gate's check passed or failed, as a [CI job](ci.md) does, naming the issue it ran on one way: `--issue ID`, `--branch NAME`, `--from-branch` (the CI system's variables, else git's branch) or `--from-commit` (HEAD's `Aries:` trailer, else the branch a merge commit merged). `--sha SHA`, `--url URL` (http or https, no user or password) and `--reason TEXT` are the evidence, at most the server's `gate_evidence_bytes` in all. `--run N` is the CI run's number, `GITHUB_RUN_NUMBER` or `CI_PIPELINE_IID` by default: a report of an older run than the gate's last changes nothing, and says so, but a failure of the commit that last passed. `--optional` exits 0, saying nothing was reported, when the branch or commit names no issue or the issue has no gate for the check. `gate pass ID` and `gate fail ID` report on a gate by its id. Only a principal the gate names reports, from a CI job (`CI=true`) or its own terminal. A failure tells the issue's holder, or its assignee |
| `land [--from COMMIT] [--branch NAME] [--dry-run] [--hook]` | After a pull of the default branch, report `landed` for each commit since the last it reported, for a server [CI cannot reach](ci.md#a-server-ci-cannot-reach). A commit names its issue as `gate pass --check landed --from-commit` reads it, else it is skipped with a note. The last commit reported is kept per project on this machine; with none, only HEAD is read, and `--from COMMIT` starts after COMMIT. It reports from the session `cli`, only from the default branch, and offline reports and keeps nothing. `--dry-run` reports nothing; `--hook`, for git's post-merge hook (`aries setup post-merge`), says nothing off the default branch and exits 0 whatever happens |
| `workflow check FILE` | Compare the project's [workflow](concepts.md#the-workflow) on the server with FILE, YAML or JSON, and exit 1, listing what differs, when they are not the same document; for any member, a CI job among them. A project admin applies a file with `aries admin workflow set FILE` |
| `gate waive ID --reason TEXT` | Pass a gate as an admin, whoever it names, on the record |
| `gate list` | List the open gates: approvers, what each holds and its title, and a ci gate's failed check; `--all` adds passed and waived ones with who and when, `-n N` |
| `label add\|remove ID LABEL...` | Add or remove labels. `rm` is accepted for `remove` |
| `comment ID TEXT` | Add a comment (`-` reads stdin) |
| `comments ID`, `history ID` | List all of an issue's comments, or all its changes; `-n N` shows only the newest N |
| `hours DURATION ID` | Log your hours on an issue ([Human hours](concepts.md#human-hours)): `1.5h`, `90m` or `1h30m`, from a minute to 24 hours, for today in your time zone or `--on DATE`, at most a year back and a day past today in UTC; `--note TEXT` adds one line. It prints the entry's id. `hours --undo ENTRY` undoes one of your entries (an admin may undo anyone's). Bare `hours` lists entries, newest day first: `--issue ID`, `--by PRINCIPAL`, 50 or `-n N` (at most 500). `aries log`, its name before 1.0, does the same, and is kept through 1.x |

### Locks

A [lock](concepts.md#locks) is a name in the project, `deploy/prod` or
`merge/main`, that up to its capacity of holders may hold at once, each
under a lease and an epoch. Use one around a deploy, a merge or a
shared resource. Agents do not take locks: a script, CI or a person
does, mostly through `lock run`.

| Command | Does |
|---|---|
| `lock run NAME -- CMD [ARG]...` | Take the lock, run CMD with `ARIES_LOCK_EPOCH` set to the hold's epoch, renew the hold while CMD runs (at a third of its lease), release it when CMD exits, and exit with CMD's exit code. `--wait` waits in the lock's queue, keeping its place every 30 seconds, rather than fail when the lock is held; `--capacity N` and `--for DURATION` as for `acquire`. CMD's output is its own, not `aries`'s, so `--json` is refused |
| `lock acquire NAME` | Take the lock and print its epoch, on its own line; stderr says how to renew and release it. `--capacity N` makes it a semaphore of N holders, at its first acquire (default 1, at most the server's `holders_per_lock`); a later acquire naming another capacity is refused. `--for DURATION` is the hold's lease (default 15m, from 1m to 24h). Without `--wait`, a lock that is held is refused with `busy`, naming its holders and when their leases end; with it, `aries` waits in the queue until the lock is granted, and gives its place up if interrupted |
| `lock renew NAME EPOCH` | Extend your hold to its lease from now, or `--for DURATION` from now. A hold that ended, or lapsed, is refused with `conflict`: stop what it guards |
| `lock release NAME EPOCH` | Release your hold; the next in the queue is granted it at once. Any session of your principal may release it, with its epoch |
| `lock leave NAME` | Give up your place in the queue; says so when you hold the lock instead |
| `lock list [NAME]` | List the locks held or waited for: how full each is, who holds it at which epoch and until when, and who waits, in order |

**How `lock run` ends.**

| When | What happens |
|---|---|
| CMD exits | The hold is released, and `aries` exits with CMD's exit code |
| You interrupt it (ctrl-c, or SIGTERM to `aries`) | CMD is sent an interrupt, and killed if it has not stopped within 10 seconds; the hold is released once it has. `aries` exits with CMD's exit code, or 130 if CMD died of the interrupt. Interrupted while waiting, it gives its place up and exits 1 |
| The server says the hold ended | Its lease ran out while the server could not be reached, or your access ended. `aries` says so, stops CMD as above, and exits 1: what the lock guarded is no longer guarded |
| The server cannot be reached for a while | CMD runs on, and `aries` keeps trying to renew, more often. If the lease runs out first, the server ends the hold and grants the lock to the next waiter; the next renewal that reaches it hears so, and stops CMD |
| `aries` itself is killed (SIGKILL, a crash, the machine) | Nothing is released. The hold lasts until its lease runs out, at most `--for` after the last renewal, and the reaper then grants the lock to the next waiter within its 30-second tick. CMD, if it outlives `aries`, runs on without the lock: a resource that checks `ARIES_LOCK_EPOCH` refuses it once the lock is granted again |

Each `lock run` takes a session of its own, `cli.lock-` and a random
suffix (or the harness's session id and the suffix), so that two runs
in one terminal are two holders; `lock acquire` holds in your session,
so two acquires of yours in the same session are one hold, and the
second gets the first's epoch.

### The dispatcher (experimental)

`aries dispatch` keeps agents at work between waves without a person
starting each session ([design](design/dispatcher.md)): it picks ready
issues by their model label, claims each, makes it a worktree, and
starts a headless Claude Code run in it under the lock `dispatch/runs`,
within a daily budget. It is internal and experimental: Bearings will
replace it, and the v1.0.0 freeze does not keep it. It runs on macOS
and Linux, and refuses on Windows. Runs act as your principal, and push
and open PRs with your own `git` and `gh`.

| Command | Does |
|---|---|
| `dispatch run` | Run the loop until stopped or interrupted. Each wake (an inbox item or issue event, at most every 10 seconds; a run's exit; the default branch moving, read every 5 minutes; a released `dispatch/pause` or a freed slot, read every 90 seconds while it holds for one; and a 20-minute timer) prints a line per decision: `start ID (model M, session S, pid P)`, `skip ID [RULE]: why`, `queue ID (model M) [slots]: why`, or `hold [RULE]: why` when nothing may start. Before them it says `judged N ready issues` (with `(the most a wake reads)` when N reached the limit): each wake reads up to 500 ready issues, the server's cap on one answer, since `ready` has no offset and a filter such as `--models` is applied after the read, and judges each with a `show`; past 500 the rest wait until ones ahead start or close. `--dry-run` prints `would start` for what it would start and starts, claims, makes and writes nothing, and needs no key; `--once` wakes once and exits, leaving the runs it started running. `--capacity N` is the lock's capacity (default 4); `--cap TOKENS` a UTC day's Opus output tokens past which it starts nothing (default 1300000); `--max-budget-usd USD` each run's limit; `--models haiku[,sonnet,opus]` starts only issues whose model label is one of these, and `--labels L[,L]` only issues carrying all of these labels (a `trial` label, say), each skipping the rest by a rule of its own and each named on a line after `wake` (`only models haiku; only labels trial`); an unknown model is a usage error that lists the known ones; `--clear` clears a stop that two CI failures in a row or an auth failure left, and lets the issues its runs failed on be picked again. Interrupted (ctrl-c), it exits and its runs keep running; the next `dispatch run` adopts them |
| `dispatch status` | Say what its state file holds: whether it runs, what holds it, today's Opus output against the cap, the CI failures in a row, the `--models` and `--labels` in force while it runs (`scope:`), and its runs, each with its issue, model, state, session, pid, PR, CI result and worktree. It never connects. `--json` prints the state as one document |
| `dispatch stop` | Ask the dispatcher running for this project on this machine to start nothing more; it exits once its runs have ended. `--now` also interrupts them (SIGINT, then SIGTERM after 30 seconds). To stop every dispatcher, on every machine, hold the lock `dispatch/pause`: `aries lock acquire dispatch/pause --for 24h` |
| `dispatch brief ID` | Print the brief for an issue, rendered from the project's template `.aries/brief.txt` as `dispatch run` renders its runs' ([design](design/dispatcher.md#7-briefs)), for a coordinator to send to a session. It reads the issue and writes nothing, and runs on every system. By default the brief is for a Desktop session (`--harness claude-code-desktop`), with a hand-written section between marked lines that `--note TEXT` fills (`(none)` without it); `--harness claude-code` prints what a headless run gets, naming `--session` and `--worktree`. `--record --model M --session S [--effort E]` prints the whole `brief:` comment to post: the first line `brief: harness=H model=M effort=E session=S template=brief@N`, a blank line, then the brief. `--json` gives the template's version, the brief, its SHA-256 (of the text trimmed, as the log and `aries outcome` take it) and, with `--record`, the comment. A missing or malformed template is refused, as `dispatch run` refuses it |
| `dispatch key set`, `dispatch key remove` | Keep the API key the runs are given in the system's credential store (the macOS keychain; the Secret Service on Linux where one runs, else a 0600 file in aries's data directory), read from standard input alone, never a terminal, or remove it. `dispatch run` reads it at start and says which store it read; it reaches each run in its environment only |

**What it skips.** An issue with no `model:haiku`, `model:sonnet` or
`model:opus` label, or more than one; the label `human` or
`waiting:maintainer`; an epic; an issue whose last handoff is `done`; one whose last handoff
is `partial` or `blocked` and whose handoff branch has an open PR
(`awaiting-review`: it waits on review, and a merged or closed PR
releases it); one whose last handoff is `blocked` and whose row has not
changed since (`blocked`: a reopen, a label change or an edit releases
it); one whose text names release signing (`internal/release`, `keys.go`,
the release workflow); one another session holds; with `--models` or `--labels`, one whose model is not listed (`only-models`) or that lacks a label (`only-labels`), after every rule above; and one a run of its
own failed on or was stopped on, until `dispatch run --clear`. Each
skip names its rule. It reads the PR with `gh pr view` on the handoff's
branch, never the handoff's text; a `partial` handoff with no open PR
and no `waiting:maintainer` label is work to continue and is started,
so a session that leaves a question for the maintainer must also label
the issue `waiting:maintainer`.

**What stops it.** Before each start it reads `aries cost` for the UTC
day: past the cap, it starts nothing more that day and lets runs end.
Two CI failures in a row on PRs its runs opened stop it, and a green
one between resets the count; the first run that fails to authenticate
stops it at once. A run past 90 minutes is interrupted. Each stop is
logged once and shown by `status`. The cap is an alert before the
provider's own: set a spend limit on the key in the provider's console.

**What each run gets.** The dispatcher claims the issue under the run's
session id before it starts the run, so a second dispatcher is refused
the claim and moves on; a launch that fails releases the claim with a
handoff saying why. The run's brief is rendered from the project's
template `.aries/brief.txt` and the issue, never by a model, and is
given on standard input and posted on the issue as a comment whose
first line is `brief: harness=claude-code model=ID effort=default
session=S template=brief@N`; the run is started with the full model id
(`claude-haiku-5-5`, not `haiku`), which the line names. Without the
template, `dispatch run` refuses before it reads anything. Its worktree is
`.aries-dispatch/worktrees/ID` at the repository's top, on the branch
`start` names; once the issue closes, the worktree is removed (never
with `--force`) and the branch deleted when its PR merged.

**What a run may run.** A run is headless under `--permission-mode
dontAsk`, so a command outside its allow list is denied, not asked.
Claude Code's allow rules such as `Bash(go:*)` do not match a command
that starts with `VAR=value`, so each run's environment carries
`GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly`, as the gate sets them,
and its brief tells it to issue commands bare and, when one is denied,
to retry it in its allowed form before giving up. The allow list covers
git, Go, the gate, and `gh pr create`,
`view`, `checks`, `list`, `ready` and `edit`; `gh pr merge`, `gh api`
and forced or `main` pushes are always denied. `mise exec` is not
allowed, nor `scripts/leftovers.sh` (the gate runs it), since each would run anything; `find` with `-exec` or `-ok` is denied.

**Where it keeps its files.** In aries's data directory, under
`dispatch/PROJECT`: `state.json` (0600), `log.jsonl` (one line per
decision: ids, rules, numbers and paths, a brief's SHA-256, never its
text, a key or an issue's text; a `--dry-run` creates no log and writes
no line), and each run's output under `runs/`.

### See what is happening

| Command | Does |
|---|---|
| `inbox` | List your unread inbox, newest first: 20 items, or `-n N` (at most 100). `--all` includes read items; `--ack ID` (repeatable or comma-separated) or `--ack-all` marks items read |
| `watch` | Print inbox items as they arrive, until ctrl-c (exit 0). With `--json`, one object per line: `{"op":"inbox","item":{…}}`, or `{"op":"resync"}` when it missed items (`aries inbox` lists them). `--events` also prints every event committed on an issue, your own included (an inbox item is never made for your own act), as `{"op":"event","event":{…}}`; after a `resync` the watch resumes with events still on, and a program that follows events rereads what it needs. It is the protocol's `watch` with `events`, as `aries tui` uses it, and needs no newer server |
| `tui` | Show the [live board](#the-live-board) until `q` |
| `whoami` | Say who the server takes you for: your principal, the project and server, the session id, and your sign-in (its provider, its end and its id) or the fingerprint of the key that signed in, with whether it came from a key file or from ssh-agent. `--json` gives the same as one document, never a token. Run it when a command acts as someone you did not expect |
| `login` | Sign in at the server's identity provider, for the day ([Sign in](#sign-in)). `--provider NAME` where the server has several; `--url` prints the URL to open in any browser and takes back the address it lands on. `--workload KIND` (or `ARIES_WORKLOAD`) signs a CI job or a cloud workload in as its service principal with the platform's token: `github-actions`, `google` or `azure`. `--json` prints the principal, provider, server, end, sign-in id and scopes, and a workload's trust rule |
| `logout` | End your sign-in at the server, closing its connections, and remove it from this machine. Says so when there is none |
| `who` | List the sessions seen in the last 5 minutes (`--since 2h`, up to 7d) and the issues each holds; at most 100 (`-n N`, up to 500), then a count of the rest |
| `digest` | Summarize a window: `--since 24h` (default), `7d`, a date or a time; filter with `--by PRINCIPAL` or `--label L`. Its sections include the issues that became ready in the window because their `defer_until` came (`ready since its date`), those due within the server's `due_soon` (`due soon`) and those past their due date (`overdue`) ([Dates](concepts.md#dates)). Under its header it prints the time issues were held (saying when some tokens were split by time), their cost at list price, the hours logged on the days the window overlaps, the tokens reported by model, and those no issue was held for |
| `cost` | Report the list-price cost of the tokens reported in a window, their amortized cost once an admin has recorded a plan, and the hours logged ([Cost](concepts.md#cost)): `--since 7d`, a date or a time (required), `--until` a date or a time (not included; default now), grouped `--group-by account` (default), `issue`, `epic`, `person`, `model` or `outcome` ([cost per outcome](concepts.md#cost-per-outcome)). It lists 50 groups, largest first, or `-n N` (at most 500), the rest summed as `(other)`, then the total. `split` marks a group whose tokens came partly from records shared by time with other work, and `unpriced` one with tokens no price covers; a group with no priced token shows `unpriced` for its cost, never $0.00. With plans, each group shows `$X amortized` after its list price; with hours, `1.5h logged` after its tokens (by `model`, people's hours are the group `(human)`) |
| `outcome ID...` | Experimental. Record what each outcome of an issue (it landed, it was closed, or its change was closed unmerged while it stayed open) led to, and post it once on the issue as an `outcome:` comment: a line for people, then one line of JSON with the CI attempts and each failure's cause, the catch-up merges of the default branch and how many conflicted, review counts, reopens, waivers, held and cycle time, tokens, and pre-merge rework ([The outcome record](design/outcome-record.md)). It reads the CI host through `gh`, signed in as you, in this repository, and fetches each change's `refs/pull/N/head` for `git merge-tree`; without `gh` on PATH those fields are listed as missing. `--since 24h`, `7d`, a date or a time records every issue that reached an outcome since then in place of IDs; `--backfill` (with `--since`) marks the records as captured after the fact. `--dry-run` prints each record and posts nothing; `--workflow NAME` (repeatable) counts only that workflow's runs as CI attempts. A record already posted with the same outcome and the same content is not posted again (`unchanged`); one whose content changed is posted as a new comment that supersedes it, naming the comment, and no comment is edited. It is CLI only, never an MCP tool, and needs no newer server |
| `prime` | Orient a session: your issues in progress, your inbox, the top ready work, memories (pinned, then relevant to your work, then newest), the issues overdue, due soon and ready in the last day by their date, and version notices. `--hook[=AGENT]` is for an agent's session-start hook ([agent guide](agents.md#session-start-hooks)) |

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
| `outbox` | List the writes that wait for the server and the ones it refused, for your principal on this machine; `outbox retry N` sends a refused entry again, `outbox drop N` removes an entry for good, and `outbox stats` shows the counts; `--host-key KEY` reaches what was queued under a host key the server had before. It never connects ([When the server cannot be reached](#when-the-server-cannot-be-reached)) |
| `cache` | Say what this machine keeps of what was read from the server for this project, shown while the server cannot be reached; `cache clear` deletes it. It never connects ([What is shown from the cache](#what-is-shown-from-the-cache)) |
| `setup AGENT` | Set an agent up for this repository (`setup post-merge` is no agent: git's hook that runs `aries land`, [offered](ci.md#a-server-ci-cannot-reach) and never written unasked); see [Set up an agent](agents.md#set-up-an-agent). With `--global`, or for Claude Desktop, `--key-file PATH` also names the key file the agent signs in with, in your own files only ([Give an agent its own identity](agents.md#give-an-agent-its-own-identity)) |
| `usage --hook[=AGENT]` | Send the token counts in the session's transcripts that earlier runs have not sent. Run by Claude Code's Stop, SubagentStop and SessionEnd hooks, which `setup claude-code` installs, with the hook's JSON on stdin; bare `--hook` is Claude Code's, the only agent captured so far. It always exits 0 and sends only counts ([Token usage hooks](agents.md#token-usage-hooks-claude-code)) |
| `admin price` | `admin price list` lists the model prices, in US dollars per million tokens. `admin price set MODEL --from DATE --input USD --output USD --cache-write USD --cache-write-1h USD --cache-read USD` sets a model's rates from a date (midnight UTC) or an RFC 3339 time, and prints `added`, `replaced` (the same model and `--from`) or `unchanged`. Every rate must be given, 0 where the model charges nothing, with at most six decimals. Setting is for admins ([Prices](server.md#prices)) |
| `admin plan` | `admin plan list` lists the subscription plans, in US dollars a seat a month, with each row's monthly total. `admin plan set NAME --from YYYY-MM --fee USD --seats N --principal P...` sets a plan's terms from a month until a later `--from` of the same name, the principals being those whose usage it pays for (repeatable or comma-separated), and prints `added`, `replaced` (the same name and `--from`) or `unchanged`. `--fee 0 --seats 0` ends a plan. Setting is for admins ([Plans](server.md#plans)) |
| `admin workflow` | The project's [workflow](concepts.md#the-workflow). `admin workflow set FILE [--rev N]` applies the workflow in FILE, YAML or JSON, which `aries` reads and checks first, naming the key a mistake is at, and prints the rev, or `unchanged` when the server has that document; `--rev N` refuses the set if a rev other than N is in force. `admin workflow show` prints the workflow in one line, who set it and when, and the document; `admin workflow diff FILE` prints what a set of FILE would change, a line for each key and item added (`+`) or removed (`-`). Setting is for the project's admins and server admins, from their own terminal, never an agent's session; showing and diffing for any member |
| `admin project` | On a server of [many projects](server.md#serving-many-projects). `admin project add NAME [--prefix P] [--account A]` creates a project, in a schema the server names for it (`project_` and NAME, with hyphens as underscores; NAME is then at most 55 characters, and a schema of that name already in the database is refused), and prints its UUID and the `.aries.yaml` to commit to its repository: this repository's server with the new project, never your key path. NAME is a code name; the prefix of its issue ids defaults to NAME and its account to `internal`. Run again after a failure, it finishes the same project. `admin project list` lists every project for a server admin, with its schema, and for anyone else the active projects they belong to, with their role; `admin project archive PROJECT` stops serving a project, closes every connection to it and releases every claim in it, keeping its data and its schema (no command removes a project); `admin project activate PROJECT` serves it again. PROJECT is a UUID or a name. Adding, archiving and activating are for server admins ([Managing access](server.md#managing-access)) |
| `admin principal` | `admin principal add NAME [--key FILE]... [--project P]` adds a principal as a member of this repository's project, or of `--project` (a UUID or a name); `--no-project`, for server admins, adds it to none. Each `--key` is a public key file to add with it, as `admin key add` takes one; the principal and its keys are added together or not at all. `--kind service --owner NAME` adds a service, which answers to NAME, a person; `--kind person`, the default, a person. `--display-name D` gives it what people see beside its name. `--invite ADDRESS --provider P`, for server admins, invites an address for it as `admin invite add` does, in the same write. `admin principal set NAME [--display-name D] [--kind person|service] [--owner NAME]`, for server admins, changes those, and prints what the principal now is, or that it already was; `--display-name ""` clears the display name. `admin principal show NAME` shows a principal's state, kind, owner and display name, its identities and its keys, to server admins and to NAME. `admin principal list` lists them all, for server admins, with a kind, an owner and a display name where a principal has one; `--created-by NAME` lists those NAME created, which a project admin may also ask, and is shown the ones in their projects. `admin principal disable NAME` stops a principal's access everywhere, closes its connections and releases its claims, and says how many; a disabled principal is refused even if it is a server admin, and the last enabled server admin cannot be disabled. Disabling undoes nothing the principal did: when NAME added keys that are still in the store, created principals or added memberships, a warning on stderr counts them and names the lists that show them (`key list --added-by`, `principal list --created-by`, `member list --added-by`), and `--json` carries the counts as `keys_added`, `principals_created` and `members_added`. `admin principal enable NAME` restores it. A project admin may add a principal to a project they administer, and disable or enable one that belongs only to projects they administer. `admin principal whois STAND-IN`, for server admins, prints whose stand-in `p_…` is: the one a server's traces carry in place of a principal's name ([Traces](server.md#traces)), as `p_0123456789abcdef is alice`, with `(disabled)` after a disabled principal's; `--json` gives `stand_in`, `name` and `state`. It is refused for a stand-in that is nobody's, and on a server that has no key for them, with what that means |
| `admin key` | A principal's public SSH keys, on a server whose sshd reads the keys file ariesd writes ([Keys from the store](server.md#keys-from-the-store)). `admin key add NAME FILE` adds the key in FILE, a `.pub` file such as `~/.ssh/id_ed25519.pub`, or `-` to read it from standard input, and prints its fingerprint. `--expires DURATION|TIME` gives the key an end, after which it signs in over neither SSH nor HTTPS and the connections it signed in are closed: a duration such as `12h` or `30d`, counted on the server's clock, or a time such as `2026-12-31T17:00:00Z`, at most ten years away; the end is printed in UTC, to the second ([Keys that end](server.md#keys-that-end)). `aries` reads the file here and sends the public key alone: a file that holds a private key, more than one line, or anything that is not one public key is refused before anything is sent. A key the principal already has changes nothing, unless `--expires` asks another end than it has, which is refused (`exists`): remove the key and add it again; one another principal has is refused, and so is one that the server's hand-maintained `authorized_keys` gives to another principal. `admin key list NAME` lists a principal's keys by fingerprint, type and comment, and when any has an end, each one's (`ENDS`), an ended key for an hour before the server prunes it: for server admins, an admin of any active project NAME belongs to (they may see the keys even where they may not change them), and NAME themselves. `admin key list` marks a key that is also a line of the server's hand-maintained `authorized_keys` file, and says how many of NAME's open connections came in by that file. `admin key list --added-by NAME` lists instead the keys NAME added, whoever each belongs to, by fingerprint (`-n N`, `--after FINGERPRINT`): everything for a server admin, and for a project admin the keys of their projects' members. `admin key remove FINGERPRINT` removes the key with that `SHA256:…` fingerprint and closes the connections it had signed in; it warns on stderr when the key is also a line of the server's `authorized_keys`: its connections were closed, and sshd, which reads that file after the server's own, still accepts the key by that line until the line is deleted on the server. Any of these commands that changes which keys sign in also warns when the server recorded the change and could not write its keys file, so that sshd may still be reading the old one. Adding is for server admins and for a project admin with reach over the principal, and never for your own account: another admin adds your keys. Removing is for the same admins, and for yourself: you may remove any key of your own except the one this connection signed in with, which is refused by fingerprint (`aries whoami` shows it; `admin key list YOU` shows the rest, with who added each). On a connection that came in by the server's `authorized_keys`, which names no key, the removal goes ahead with a warning that it could not be checked |
| `admin invite` | On a server of [many projects](server.md#serving-many-projects), the invitations through which a person's sign-in at the company's identity provider becomes a principal ([Identities and invitations](server.md#identities-and-invitations)); for server admins. `admin invite add NAME ADDRESS --provider P` invites ADDRESS at provider P for the person NAME and prints the invitation's id and when it lapses unused (`login: invite_ttl`, 7 days by default); the same address for the same principal again renews it, and an address another principal is invited as is refused, naming that invitation. Nobody invites onto their own principal. `admin invite list [--pending]` lists the invitations no sign-in has bound, by id, with their state, `pending` or `lapsed`, and `--pending` only those still open (`-n N`, `--after ID`). `admin invite remove ID` removes one; a bound identity is refused, naming `admin identity remove` |
| `admin identity` | A principal's identities, invited and bound. `admin identity list NAME` lists them by id, with provider, address, state (`pending`, `lapsed` or `bound`), when each was invited or bound and when an invitation lapses: for server admins, and for NAME. `admin identity remove ID`, for server admins, removes one, bound or not, and ends the sign-ins made with it, closing their connections |
| `admin signin` | A principal's [sign-ins](#sign-in). `admin signin list NAME` lists them by id, with provider (and a service's trust rule), machine, scopes, state (`live`, or `ended` and why: `logout`, `revoked`, `identity removed`, `principal disabled`, `trust rule changed`, `trust rule removed`) and when each began and ends: for server admins, and for NAME. `admin signin end ID` ends one now and closes its connections: for server admins, and for its holder. Never a token |
| `admin trust` | The trust rules by which a service principal signs in with its platform's token ([Service principals](server.md#service-principals)). `admin trust add NAME --principal SERVICE --provider P --claim NAME=VALUE… [--token-lifetime D]` adds one: SERVICE a principal of kind `service`, P a workload provider of the server's settings, and each claim one the platform's token must carry with exactly that value, the platform's immutable ids among them (`repository_owner_id` and `repository_id` for GitHub Actions, `sub` and `email` for Google, `tid` and `oid` for Azure), which the server checks. `admin trust list [SERVICE]`, `show NAME` (with the claims the provider requires), `set NAME [--claim NAME=VALUE…] [--token-lifetime D]` (`--claim` replaces every claim), `disable NAME`, `enable NAME`, `remove NAME`. Changing a rule's claims, disabling or removing it ends the sign-ins it gave and closes their connections. Adding and changing are for server admins, never for one's own principal; listing and showing for server admins and the service's owner |
| `admin member` | `admin member add NAME [--project P] [--role admin] [--accept-keys]` makes a principal a member of the project, or changes its role (`member`, the default, or `admin`), and prints `added`, the new role, or that nothing changed. A key signs in to every project its principal is in, so when NAME has any key and is not yet a member, nothing is added at first: `aries` lists every key NAME has, with who added it, when, and whether that was an admin of this project, and on a terminal asks before it adds. Without a terminal it exits 1 with a fix naming `--accept-keys`, which adds without asking; with `--json` the one document holds the refusal under `error` and the keys under `member`. Someone with no key is added at once, and a change of role asks nothing. `admin member list [--project P]` lists the project's members; with `--added-by NAME` it lists the memberships NAME added, in every project you may list the members of, or in `--project`, by project and principal (`--after PROJECT/PRINCIPAL` continues). `admin member remove NAME [--project P]` ends a membership, closes that principal's connections to the project and releases the claims it held there, saying how many; it warns on stderr when that leaves the principal within reach of project admins who could not manage it before. For the project's admins and server admins. Each `admin` list of projects, principals or members is by name, 200 rows or `-n N` (at most 1000), then a count of the rest, which `--after NAME` lists |
| `admin status` | What the server knows of itself and its projects now ([Watching a server](server.md#watching-a-server)). A server admin sees the server (version and protocol range, uptime, open stores and connections beside their limits, the keys file it writes for sshd and whether its last write failed, the times its clock was set and whether it is behind the server's own history ([The server's clock](server.md#the-servers-clock)), ariesd's own memory and goroutines, Postgres's own figures and ariesd's pool of connections to it, projects and principals by state, and identities as bound, pending or lapsed once there are any) and a row for every project; a project admin sees the rows of the active projects they administer; anyone else is refused. A row gives the project's state, whether its store is open, its schema's size on disk, its connections and how many of them came in by the hand-maintained `authorized_keys`, the sessions seen in five minutes, the writes of the last minute, and the median and 99th percentile latency of its latest 256 writes. `-n N` and `--after NAME` page the projects. It changes nothing |
| `upgrade` | Replace `aries` with the latest release after checking its signature and checksum. `--check` reports and changes nothing; `--rollback` restores the binary the last upgrade replaced. It never runs on its own |
| `version` | Print the version (`--json` adds the protocol) |

## The live board

`aries tui` fills the terminal with a board that keeps itself current:

- **Ready**: the issues nothing holds back, best first, up to 100. An
  issue whose files overlap work another session holds is marked
  `[overlaps ID]` ([Files](concepts.md#files-what-an-issue-touches)).
- **Held**: each issue under a live claim, with its holder as
  `principal/session` and how long they have held it, on the server's
  clock. `[lapsed]` marks a lease that ran out before the reaper freed
  the issue.
- **Blocked**: the issues held back by open blockers, `[by ID +N]`, and
  parents waiting on their children, `[N open children]`.
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

`aries tui` needs a terminal on standard input and output, and one that
does not say `TERM=dumb`. Anywhere else it refuses and points at
`aries ready`, `aries blocked` and `aries list`. It
honors [`NO_COLOR`](https://no-color.org): set, the board draws without
color and marks the selection with `>` alone. Like all output, text from
the server is escaped (below).

## When the server cannot be reached

A server restarts, a tunnel or a VPN drops, a database is slow. When a
write cannot reach the server, `aries` keeps it in an **outbox** on your
machine and sends it later: `create`, `comment`, `handoff`, `remember`,
`hours` (logging hours), `finish`, `update`, `label add` and `label remove`,
`dep add`, `accept`, and token usage. Each carries a key that lets the
server apply it once however often it is sent.

**When sshd hangs up.** A server that accepts the connection and closes
it before the SSH handshake is not reachable either, and a write made
then is queued. `aries` says so, as `HOST:PORT closed the connection
before the ssh handshake`: sshd does that for a while to an address
with failed or dropped connections (`PerSourcePenalties`, OpenSSH 9.8
and later) or past `MaxStartups`, and a firewall or proxy can too. Wait
a minute and try once, not in a loop, since each attempt can lengthen
the penalty; check that `aries whoami` signs in with the key the server
knows; and if it persists, ask the server's operator to look for
`penalty` in sshd's log ([Troubleshooting](server.md#troubleshooting)).

Reads have a **cache**: what you read of your own work is kept, and
shown while the server cannot be reached, saying so
([What is shown from the cache](#what-is-shown-from-the-cache)). A
queued `update` that arrives after someone else changed the same field
is not refused: the server applies what nobody else changed and parks
the rest as a [conflict](#conflicts) for a person to resolve. A queued
`finish` or `handoff` that arrives after its claim lapsed is not
refused either: see a late finish, below. Any other
queued write that no longer fits is refused when it arrives, and waits
for you. And a write waits 7 days at most, the server's window for a
key (`idem_window` in [the server's limits](server.md#limits)).

**`update` needs its rev.** An `update` is queued only with the rev it
was based on, which offline only you know: `aries update ID --rev N ...`.
Without `--rev` it first reads the issue's rev from the server, and with
the server away it fails, saying to give `--rev`.

**A late finish.** Your claim's lease runs out on the server's clock
while you cannot reach it, unless `aries away` extended it first; after
that the reaper reopens the issue and anyone may take it. A queued
`finish` or `handoff` that arrives after its lease ran out is decided
on what happened meanwhile:

| Since the lease ran out | What happens to the queued write |
|---|---|
| Nobody took the issue | It applies as it would have; the issue's history has a `claim.lapsed` event first, saying the lease had lapsed |
| Someone took it, you in another session among them, or it was closed | It is kept as a comment on the issue, saying why, with its reason, note and fields; a `finish`'s discovered issues are still filed, discovered from the issue; nothing else of it applies. You get a `conflict` item in your inbox, every session of yours, and the session that holds the issue gets one too |

It is delivered either way, and `aries` says how many were kept:
`outbox: delivered 2 (1 kept as a comment, as the issue moved on while it waited: `aries inbox` says why)`.
Only a write replayed from the outbox is treated so: a `finish` or
`handoff` you run while the server answers is refused, as always, when
another session has taken the issue since your epoch.

**A server of another protocol.** `aries` speaks protocol 9 (v0.6.0),
and a server of this release serves clients of 8 and 9, so v0.5.0's
`aries` keeps working against it. A server that does not accept 9,
such as v0.5.0's, refuses the connection with `version` (exit code 3)
and says which side to upgrade: have the server upgraded first. What
waits in the outbox stays there, unsent and not refused, until a server
that speaks 9 is reached.

**What counts as not reached.** The connection could not be made (the
network, the tunnel, sshd, or `ariesd` not running), it was lost
before the answer came, the answer did not come within 75 seconds, or
the server answered `unavailable`. The server gives up on a request
after a minute and says so, so `aries` waits 75 seconds before it closes
a connection that has gone silent and fails the command (`the server
did not answer show within 1m15s, so aries closed the connection`). The
`aries admin` commands that change projects, principals, members, keys and identities
are waited on for as long as they take, since the server may take
longer over them; interrupt one with ctrl-c. A refusal
is not an outage, and fails as it always has: an unknown key, a host key
that does not match its pin, `forbidden`, `invalid`, `not_found`,
`conflict`, a protocol version, a mistake in `.aries.yaml`, and the
server's rate limit (`busy`).

**What you see.** One line, and exit code 0, since the write was
accepted for delivery:

```text
$ aries handoff ar-a1b2c3d4 "half done" --next "the tests"
queued (server unreachable since 2026-10-13 14:02 UTC): entry 3, handoff ar-a1b2c3d4; aries sends it when the server answers (`aries outbox` lists what waits)
```

With `--json` it is one document, which never has the `id` and `rev` of
a write that was made:

```json
{"queued":true,"entry":3,"op":"handoff","issue":"ar-a1b2c3d4","since":"2026-10-13T14:02:00Z"}
```

`issue` is the issue the write is about, and a `remember` has `key`
instead. A queued `create` has neither, and a `note`: the issue has no id
until it is sent, so nothing else you queue can refer to it. A `label`
command of several labels queues a write for each, prints a line for
each, and with `--json` one document, `{"queued":true,"entries":[...]}`.

**Scripts.** A script that reads an id from `aries create`, or that must
know the write was made, passes `--no-queue` or sets
`ARIES_NO_QUEUE=1`: the write then fails with exit code 1, as it did
before there was an outbox.

**What is not queued.** Everything else fails as before, and for a write
the `fix:` line says that it was not queued and why. `start` takes a
claim, and no claim is made without the server: while it is away you
finish what you hold and file new work, and start nothing new. `close`,
`reopen`, `forget`, `dep remove` and `hours --undo` end or undo something on a
state your machine cannot see. No `gate` command is queued: adding one
later could hold back work someone started meanwhile, a pass is your
decision at the moment you make it, and a CI job's report belongs to
its run, so the job fails instead. No `lock` command is queued, and
`lock run` runs nothing, so that a script fails before its critical
section, not inside it ([Locks](#locks)). The hooks (`prime --hook`,
`usage --hook`) queue and send nothing; `prime --hook` does show prime
from the cache.

The outbox itself may refuse a write, which then fails with exit code 1
and a fix:

| Refusal | Code | What to do |
|---|---|---|
| This machine has never reached the server with this key, so it cannot tell whose write it is | `unavailable` | Run it again when the server answers. Writes are queued once any command of yours has reached the server from this machine with this key |
| The outbox is at its cap: 1,000 entries, or 4 MiB | `busy` | Run it again when the server answers, which sends what waits; or `aries outbox drop N` what is no longer wanted |
| One write is larger than 256 KiB | `invalid` | Shorten its text, or run it again when the server answers |
| The text looks like it holds a credential | `invalid` | Remove the secret, and name where it is kept instead. The outbox is a file on your disk and keeps no secret; this check is the one the server runs on memories, here on every queued write |
| The data directory is not yours alone, or cannot be written | `unavailable` | `chmod 700` it if it is yours, or name another with `ARIES_DATA_DIR` |

**Delivery.** The next `aries` command that connects sends what waits
first, in order, before its own request, for five seconds at most; what
is left goes with the command after. When anything was sent, one line on
stderr says so:

```text
aries: outbox: delivered 3, refused 1 (see `aries outbox`)
```

An answer from the server settles an entry. Applied, it is gone.
Refused, it moves to the refused list with the server's code, message
and fix, and is never sent again by itself. If the server still cannot be
reached, or says `busy`, the entry stays where it is with everything
after it. An entry that waited more than 7 days is not sent: it moves to
the refused list as `stale`, so that a write that late is sent only when
you say so. `aries outbox retry N` sends it, with its key: one the server
never had applies; one it applied more than its window ago is refused
by the server as `stale`, saying the write was applied then, and is
never applied again. If you still want that change, make it again by
hand. A write sent again within the window gets the answer it got the
first time, and is not applied twice.

**When your access has ended.** If the server refuses you the project
when a command connects, as `forbidden` (you were disabled, removed from
the server, or are no longer a member of the project), what you queued
for it can no longer be sent. Every waiting entry moves to the refused
list at once, with the server's message and fix, and one line on stderr
says so:

```text
aries: alice is not a member of project "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
aries: outbox: 2 writes queued as alice were refused with it, and kept: `aries outbox` lists them, to retry once access is back or drop
```

One of those entries may already have been applied. A replay the server
answered `busy` is sent again, and if that attempt reached the server but
its answer was lost before your access ended, the server holds the write
while your outbox lists it as refused. `aries` cannot ask once you are
no longer let in. Retrying it is safe once your access is back: the
write keeps its idempotency key, so a retry of one already applied gets
its first answer within the server's `idem_window`, and is refused as
`stale` after it, saying it was applied; a key is never applied twice.

Only that refusal does this, and only when the server names you in it.
A project that is archived is refused as not served, which the
project's activation mends, so what you queued for it goes on waiting.
A key the server no longer holds is a refusal of the key, not of you,
and the entries wait for one of your other keys.

An entry is sent by whichever connection of yours comes next, and the
server records that connection's session and machine on it, not those of
the session that wrote it. So when your terminal delivers the `finish`
an agent's session queued, the history shows session `cli`, and the
agent's session finds a `claim.lost` item in its inbox for the issue.
The write itself is the one that was queued.

**`aries outbox`** lists what waits and what was refused, and works with
the server away:

```text
$ aries outbox
alice's outbox: 2 waiting, 1 refused
server unreachable since 2026-10-13 14:12 UTC; last tried 2026-10-13 14:32 UTC
waiting:
  N  OP       ON           AGE  FROM
  2  handoff  ar-a1b2c3d4  25m  cli on laptop
  3  create   -            5m   cli on laptop
refused:
  1  comment  ar-9z8y7x6w  35m  cli on laptop
     not_found: issue ar-9z8y7x6w not found
     fix: find the id with `aries list`
`aries outbox retry N` sends a refused entry again; `aries outbox drop N` removes an entry for good
```

| Command | Does |
|---|---|
| `outbox` | List the entries: number, op, the issue's id or the memory's key, age, and the session and machine that wrote it; for a refused one the code, message and fix. And when the server was last not reached, and what waits under host keys the server had before |
| `outbox retry N` | Make refused entry N wait again. The next command that connects sends it, with the key it always had |
| `outbox drop N` | Remove entry N, waiting or refused, for good. It is deleted from the outbox and never sent |
| `outbox stats` | For each op, the entries queued, delivered (and of those, parked: see [Conflicts](#conflicts)), refused, stale and dropped, and how long the delivered ones waited; and the outages: how many, how long in all, the longest, and the one in progress. With `--json` the durations are in nanoseconds |

Each takes `--json`, and `--principal NAME` for the outbox of another
principal this machine has queued as.

**When the server's host key changes.** A host key rotated as [the
server guide](server.md#rotating-the-host-key) has it changes nothing
here: while `.aries.yaml` pins the old and the new key, the server is
one server whichever it presents, and once a command of yours has
reached it in that time, what you queued stays yours to send after the
old key is removed. A server rebuilt with a new host key, or one whose
new key was pinned without a command reaching it while both were
pinned, is, to `aries`, another server until you confirm the key and pin
it. What you queued under the old key is then left where it is, and
nothing of it is sent by itself: a changed host key is for a person to
look at. `aries outbox` lists it, and `prime` has a notice:

```text
alice's outbox is empty
no failure to reach the server is recorded
under the server's old host key SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU, at example.com:22: alice's outbox, 2 waiting, 0 refused
  not sent by themselves, since the server's host key changed: `aries outbox --host-key SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU` lists them
```

| Command | Does |
|---|---|
| `outbox --host-key KEY` | List what waits under the old host key `KEY`, as `outbox` lists your own |
| `outbox retry N --host-key KEY` | Move entry N, waiting or refused, to this server's outbox, with the key it always had: the next command that connects sends it. Only when `.aries.yaml` names the server at the address it had when the entry was queued (`server.host`, `server.port` and the IAP route); otherwise it is refused, and the entry can only be dropped |
| `outbox drop N --host-key KEY` | Remove entry N for good |

### Conflicts

A queued `update` carries the rev it was based on. When it arrives
after someone else changed the issue, the server does not refuse it:
it compares each field the update sets with the issue as it was at that
rev, which its history gives ([design §18.7](design/aries.md#187-conflicts-parked-on-the-server)).

- A field nobody else changed since is applied, and so is one the other
  change set to the same value, or set back to what it was.
- A field someone else changed is **parked** as a conflict, with three
  values: base (the field at your rev), mine (your update's) and theirs
  (the server's now). The server's value stays until a person resolves
  it. Long text (body, design, acceptance, notes) parks whole: there is
  no merge of lines.
- Status and assignee are the server's: a change to them since your rev
  keeps the server's, and yours is dropped. So are declared paths.
- An issue closed since your rev takes nothing: the update is kept as a
  comment on it, listing what it would have set.
- Labels, dependencies, comments and checklist ticks are not compared:
  they add to what is there.

The replay says so on stderr, as `aries: outbox: delivered 1 (1 parked
conflicts for a person to resolve: `aries conflicts`)`; an inbox item
tells your principal, in every session, and the holder of the issue
when that is another principal; and `prime` counts the open conflicts
on your work. `aries outbox stats` counts parked entries among the
delivered ones.

```text
$ aries conflicts ar-a1
#3 ar-a1 title: alice's update from rev 4, parked 2h ago, against bob's of 3h ago
  base:   Fix login
  mine:   Fix the login redirect
  theirs: Fix login and logout
  advice: theirs is from bob, who holds the issue; take theirs
  proposed by agent-7: text: Fix the login and logout redirects (keeps both)
$ aries resolve 3 --accept
ar-a1 rev 9
```

The advice is worked out when you read it, from who holds the issue
and who made each side; it is never applied by itself. An agent may
**propose** a resolution, kept with the conflict, a later one
replacing it ([agent guide](agents.md#conflicts)); only `aries resolve`
decides, `--accept` taking the proposal. A later change to the same
field, by anyone, closes the conflict, noted in the issue's history.
`digest` lists conflicts open longer than the server's `conflict_age`
(a day by default). The server caps the open conflicts on one issue
and those one principal parked ([limits](server.md#limits)); an update
that would pass a cap is refused, nothing of it applied, and waits in
`aries outbox` until the conflicts are resolved and it is retried.

A live `update` with `--rev` is not compared: if the issue changed, it
is refused with `conflict`, and you read it again.

### What is shown from the cache

Every read the server answers that is part of your own context is kept
on your machine as it was returned, with when: `prime`; `show` of each
issue you looked at, the latest answer for each issue; the issue
`start` took, kept as a `show` of it; `inbox`; and `digest`, for the
arguments it was run with. Agents' reads through `aries mcp`, and the
SessionStart hook's prime, are kept the same way, in the same place, so
what your agent read you can read too.

The cache is shown only when the server cannot be reached (as above),
never in place of a server that answers. Then `show`, `prime`, `inbox`
and `digest` print what was kept, exit 0, and say on stderr, in one line,
that it is this machine's copy and how old:

```text
$ aries show ar-a1b2c3d4
aries: cached: aries cannot reach its server, so this is this machine's copy from 2026-10-13 14:02 UTC (3h old), and may be out of date
ar-a1b2c3d4  P1  in_progress  task  rev 4
...
```

Standard output is the command's own, as when the server answers. With
`--json` the document has `"cached": true` and `"cached_at"`, the time
it was kept, and nothing is printed on stderr. A read that was never
kept, with these arguments, fails as before.

**prime from the cache** has your work in progress, your memories, your
inbox and the notices of your outbox, and a first notice that it is
cached. It leaves out the ready issues, and says why: nothing can be
started without the server, and what is ready now is not known. So does
`prime --hook`, which still exits 0 and prints one JSON document, now with
the cached context in it.

**Refused, not shown.** `ready`, `blocked`, `list`, `who`, `gate list` and `lock list` are answers
about what everyone is doing now, and from a copy they would be wrong in
the way that has two sessions take the same work. With the server away
they fail as before, and say when the server was last reached:

```text
$ aries ready
aries: ariesd is not running on the server; ready is not answered from this machine's copy, since it says what others are doing now (server last reached 2026-10-13 14:02 UTC)
fix: ask the server admin to start it (`systemctl start ariesd`); meanwhile `aries prime` and `aries show ID` answer from this machine's copy of what you read
```

There is no flag to show them from the cache anyway: they are not kept.

**Whose.** One cache for each server, project and principal, chosen while
the server is away by the rule that chooses an outbox (below): the
principal the server last took your configured key for, or with none
configured the one it took the last time. Another principal using the
same machine and data directory is never shown yours.

**Bounds.** For each principal: 500 answers, 16 MiB, 1 MiB for one
answer (a larger one is not kept), and 7 days. Each write removes what is
past them, the oldest first. They are fixed in `aries`
(`internal/offline/cache.go`), since what grows is on your own disk.

**Deleted.** When a command's connection is refused because your access
to the project has ended (as for the outbox, above), your cache of the
project is deleted from the machine, and one line on stderr says so:

```text
aries: cache: 12 answers alice read from this project were deleted from this machine
```

| Command | Does |
|---|---|
| `cache` | Say what is kept for this project on this machine: for each principal, the answers, their size and when they were kept; and when the server was last reached |
| `cache clear` | Delete it, every principal's, for this project. The outbox is left alone |

Both take `--json`, and neither connects.

### Where it is kept

Under your data directory, never in the repository:

| System | Directory |
|---|---|
| Linux | `$XDG_DATA_HOME/aries`, or `~/.local/share/aries` |
| macOS | `~/Library/Application Support/aries` |
| Windows | `%LocalAppData%\aries`: the local application data directory, never the roaming one |

`ARIES_DATA_DIR` names another directory, by its absolute path. It
must be on a local disk that is not synced: the locks that keep hundreds
of sessions from each other's writes do not cross machines.

There is one outbox, and one cache, for each server, project and
principal. The server
says who a key is, so `aries` notes, at each connection, the principal the
server took the key for, and uses that while the server is away:

- With a key configured (`ARIES_KEY_FILE`, or `key:`), the write goes
  to the outbox of the principal the server last took that key for.
- With none configured, it goes to the outbox of the key the server
  accepted the last time none was configured, as long as that key is
  still among those `aries` would offer.
- Otherwise nothing is queued.

An outbox is sent only on a connection the server takes for its own
principal. If the server has since given your key to another principal,
the entries stay, unsent, and `aries` says so once on stderr, with what to
do; `aries outbox` goes on listing them. A new key of the same principal
finds the same outbox.

The directory must be yours alone: on Linux and macOS `aries` refuses one
that another user owns, or that the group or others can read or write,
and tells you to `chmod 700` it; it does not change the mode itself.
[The outbox](security-model.md#the-outbox) in the security model says
what is in it.

**Windows.** `aries` builds for Windows, and the outbox has never run
there: its file locks and renames are untested on it, and the check that
the directory is yours alone is not made, since Windows keeps that in
access control lists.

## Exit codes

| Code | Means |
|---|---|
| 0 | Success, help asked for (`aries help`, `--help`, `-h`), or a write that was queued for the server ([above](#when-the-server-cannot-be-reached)); `--no-queue` makes that a failure |
| 1 | Failure. stderr has a `fix:` line saying what to do next. When the server itself failed the request (`unavailable`), a last line, `req:`, gives the id the server logged it under: quote it to whoever runs the server. `--json` has it as `error.req` |
| 2 | Usage error: an unknown flag or a missing argument, or a bare `aries`, which prints the help |
| 3 | The server refused this client's protocol version; the `fix:` line says which side to upgrade |

**`lock run` exits with its command's code**, so that a script or a CI
step sees what the command did: once CMD has run, a 1, 2 or 3 is CMD's,
not `aries`'s. It exits 130 when an interrupt (ctrl-c, SIGTERM) ended
CMD, and 1 when the hold was lost while CMD ran, after stopping it,
since what the lock guards was no longer guarded. Before CMD starts, the
codes are `aries`'s own, as above: 1 when the lock could not be taken, 2
for a mistake on the command line ([Locks](#locks)).

With `--json`, a failure prints one document on standard output and
nothing on standard error, in the shape the MCP server gives an agent:

```text
$ aries --json -C /tmp show ar-a1b2c3d4
{"error":{"code":"invalid","message":".aries.yaml not found here or in any parent directory","fix":"run aries inside a repository that has one, or pass -C DIR; the search stops at the repository's top level and at your home directory, and a linked worktree reads the main worktree's"}}
```

`code` is one of the codes named in this guide (`invalid`, `not_found`,
`forbidden`, `busy`, `unavailable` and the rest), `message` names the
cause and `fix` the next step; `fix` is absent only when there is none
to name. `req`, the id the server gave the request, is there when the
server sent one, as above. A few refusals add a field of their own beside
`error`, as `admin member add` adds `member`.

Every document `--json` prints, a refusal's included, only gains fields
within 1.x: a field may be added to any of them, and none is removed or
changes its type. A script should ignore the fields it does not know.
`version --json` gives `version`, the release, and `protocol`, the
protocol it speaks, as a number.

A refusal exits 1 like any other failure, and `--json` gives its code.
When another principal holds an issue, a change to it is refused with
`forbidden`, as is a change reserved for admins, and a connection to a
project you are not a member of. `start` is refused with
`conflict` instead, and names the next ready issue. A request past one of
the server's limits is refused with `invalid` or `busy`
([Limits](server.md#limits)). When your sessions already hold every
connection the server allows one principal, the command cannot connect
at all, and nothing it writes is queued:

```text
$ aries list
aries: alice is at its connection limit (64)
fix: close some of your other aries sessions (each running aries mcp, aries watch or aries tui holds a connection), or wait for them to end, and retry; or ask the server's operator to raise conns_per_principal under limits: in ariesd's config
```

## Text from others is shown safely

Issue text comes from other people and their agents. `aries` prints any
control or bidirectional character as a visible escape (`\x1b`,
`\u202e`), so text cannot clear your terminal or disguise itself as
`aries`'s own output. [Security](security-model.md#untrusted-text) has the full
rules.
