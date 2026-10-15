# Installing starfix

starfix has two binaries. Everyone who uses starfix installs `sfx`; only a
server needs `starfixd`.

| Binary | Runs on | Installed by |
|---|---|---|
| `sfx` | Linux, macOS and Windows, on amd64 and arm64 | each person, on each machine where they or their agents work |
| `starfixd` | Linux, on amd64 and arm64 | the server's admin ([Running a server](server.md#3-install-starfixd)), or the server installer, below |

## Install sfx

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
```

The script installs the latest `sfx` into `~/.local/bin` and prints the
line to add if that directory is not on your `PATH`. It never uses sudo or
edits your shell files. Before it installs anything, it checks:

- the release's Ed25519 signature, with OpenSSL 3 (without it, the script
  warns and checks only the sha256);
- the archive's sha256.

Options go after `sh -s --`, for example
`curl -fsSL … | sh -s -- --version v0.2.0`:

| Option | Variable | Does |
|---|---|---|
| `--version vX.Y.Z` | `STARFIX_VERSION` | Install that release instead of the latest |
| `--dir DIR` | `STARFIX_DIR` | Install into DIR instead of `~/.local/bin` |
| `--server` | | Install `starfixd` instead (Linux only) |
| `--require-signature` | | Fail, rather than warn, when OpenSSL cannot check the signature |
| | `STARFIX_RELEASES` | Read releases from a mirror instead of the default ([below](#where-releases-are-read-from)) |

On Windows, download the zip from the
[releases page](https://github.com/ariesworx/aries/releases) and put
`sfx.exe` on your `PATH`, or run the script inside WSL.

**First run.** `sfx` needs no configuration of its own beyond the
repository's `.starfix.yaml`. It signs in with the SSH key `ssh` would
use: a default file in `~/.ssh` (`%USERPROFILE%\.ssh` on Windows), such
as `id_ed25519`, and then the keys in ssh-agent. Send the admin that
key's `.pub`, then run `sfx whoami` in the repository: it prints your
principal and which key signed in. To sign in with another key, set
`STARFIX_KEY_FILE` to the file's path
([Your key is your identity](cli.md#your-key-is-your-identity)).

## Install a server

On a fresh Ubuntu 24.04 LTS or 26.04 LTS machine, `install-server.sh` sets up the
whole server, `starfixd` included, as root:

```sh
curl -fsSLO https://raw.githubusercontent.com/ariesworx/starfix/main/install-server.sh
sudo sh install-server.sh --admin alice --admin-key alice.pub --host starfix.example.com --dry-run
```

It installs `starfixd` and `sfx` with `install.sh`, so they are checked
the same way, and reads `STARFIX_RELEASES` or `--releases` for a mirror.
Drop `--dry-run` to set the server up.
[With the installer](server.md#with-the-installer) has its options, its
steps and what it leaves to you.

To have Claude Code do this from your workstation, and run the server
afterwards, with every change shown to you before it runs, install the
`starfix-ops` plugin from this repository
([Operate a server with Claude Code](server.md#operate-a-server-with-claude-code)).

## Download by hand

Each [release](https://github.com/ariesworx/aries/releases) carries:

- `sfx` for Linux, macOS and Windows, and `starfixd` for Linux, on amd64
  and arm64, as `<binary>_<version>_<os>_<arch>.tar.gz` (`.zip` on
  Windows);
- `checksums.txt`, the sha256 of every archive;
- `checksums.txt.sig`, the Ed25519 signature of `checksums.txt`.

Unpack the binary onto your `PATH`, after you
[verify](#verify-a-download-by-hand) the download.

## Where releases are read from

`install.sh`, `sfx upgrade` and `starfixd upgrade` read releases from
GitHub unless told otherwise. One environment variable,
`STARFIX_RELEASES`, tells all three:

| `STARFIX_RELEASES` | Reads |
|---|---|
| unset | The default built into the binary or the script: GitHub today |
| `github` | GitHub |
| `https://releases.example.com/starfix` | A static tree on a host that serves files over HTTPS |
| `/srv/starfix-releases`, or `file:///srv/starfix-releases` | The same tree in a directory: an absolute path |

A static tree is a file `index.json` and one directory for each release,
named by its tag, holding the files listed under
[Download by hand](#download-by-hand)
([RELEASING.md](../RELEASING.md#publishing-to-a-static-host) has the
layout). On a server, the config file's `releases:` does the same for
`starfixd upgrade` ([Upgrades](server.md#upgrades)).

**A mirror is not trusted, and needs no key.** Whatever the source, a
release is installed only when the signature on its `checksums.txt`
verifies against the release key built into the binary or the script,
and the archive's sha256 is in that file. A mirror that serves anything
else is refused. Over HTTP the address must be `https://`; plain `http://`
is accepted only to `127.0.0.1` or `[::1]`. No redirect is followed, and
the address may hold no user name, password or query.

What a mirror, like any source, can do is withhold: one that goes on
serving an old `index.json` keeps `sfx upgrade` saying `up to date`.
`sfx` never installs a release older than the one it is. `install.sh`
has nothing to compare with and installs the newest release the index
lists, so pass `--version` when you know which you want.

**Without a network.** Carry a release's files in on any medium: the
archives you need, `checksums.txt` and `checksums.txt.sig`. Put them in
a directory named by the tag, and name its parent:

```sh
mkdir -p /srv/starfix-releases/v0.6.0
cp sfx_0.6.0_linux_amd64.tar.gz checksums.txt checksums.txt.sig /srv/starfix-releases/v0.6.0/
STARFIX_RELEASES=/srv/starfix-releases sh install.sh --version v0.6.0
```

A release named by `--version` needs no `index.json`. `sfx upgrade`
looks for the latest release and so needs one; from a checkout,
`go run ./internal/tools/releaseindex v0.6.0 DIR /srv/starfix-releases`
copies the release's files from DIR and writes it. To serve one mirror
to many machines, put the directory behind any HTTPS file server and
set `STARFIX_RELEASES` to its URL.

## Build from source

```sh
go install github.com/ariesworx/aries/cmd/sfx@latest
go install github.com/ariesworx/aries/cmd/starfixd@latest
```

Each builds the same code, unsigned, into `$(go env GOBIN)` or
`$(go env GOPATH)/bin`. Under a version manager such as mise, that is the
manager's own directory, which changes with the Go version. Prefer the
script. On a server, put `starfixd` where
[step 3](server.md#3-install-starfixd) installs it.

## Verify a download by hand

Work from a checkout of the release's tag, so the public key comes from the
repository rather than from the download. For release v0.2.0 on Linux:

```sh
git clone --branch v0.2.0 https://github.com/ariesworx/aries
cd starfix
gh release download v0.2.0 -p checksums.txt -p checksums.txt.sig -p 'sfx_0.2.0_linux_amd64.tar.gz'
go run ./internal/tools/releasekey verify checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify sfx_0.2.0_linux_amd64.tar.gz --repo ariesworx/starfix
```

| Command | Checks |
|---|---|
| `releasekey verify` | The signature on `checksums.txt`, against the keys in `internal/release/keys.go` |
| `sha256sum -c` | The archive against `checksums.txt` (on macOS, `shasum -a 256 -c`) |
| `gh attestation verify` | GitHub's build provenance: the archive was built by this repository's release workflow |

Releases are signed in CI with a key that only the maintainer's approved
release runs can use. [RELEASING.md](../RELEASING.md) has the details and
the trade-offs.

## Upgrade and roll back

| Command | Does |
|---|---|
| `sfx upgrade --check` | Print one line: your version, the latest, and whether to upgrade. Changes nothing |
| `sfx upgrade` | Replace `sfx` with the latest release, after checking its signature and checksum |
| `sfx upgrade --rollback` | Restore the binary the last upgrade replaced |

`sfx upgrade` never runs by itself. When the server runs a newer version
than your `sfx`, each command prints a one-line notice, and `prime`, which
starts an agent's session, includes it. To upgrade the server, see
[Upgrades](server.md#upgrades).

`sfx upgrade` reads the release from GitHub, or from the mirror
`STARFIX_RELEASES` names ([Where releases are read
from](#where-releases-are-read-from)), and asks your starfix server
nothing, so it works when the server refuses your `sfx` as too old.

**Coming from v0.4.x or earlier.** A server of this release refuses every
earlier `sfx`, once, on purpose ([Upgrades](server.md#upgrades)). Each
command then exits 3 and says:

```text
sfx: starfix speaks protocol 4; the server (v0.5.0) needs protocols 7 to 8
fix: upgrade this client: run `sfx upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `sfx mcp`
```

Run `sfx upgrade`, or the install script above, on each machine. Then
restart your agents: the MCP server an agent's harness started is the
old `sfx` until the harness starts it again, and its tools go on failing
with code `version` until then. If your `sfx` is the new one and the
server is not yet, you are told to have the server upgraded instead, and
`sfx upgrade --rollback` puts your previous `sfx` back meanwhile.

## Uninstall

1. In each repository you set up, run `sfx setup --all --remove`. Add
   `--global` if you set agents up for every repository, and run
   `sfx setup claude-desktop --remove` if you registered Claude Desktop.
2. Delete `sfx`, and the `sfx.prev` that an upgrade leaves beside it, from
   `~/.local/bin` or wherever you installed it.
