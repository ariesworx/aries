# Installing Aries

Aries has two binaries. Everyone who uses Aries installs `aries`; only a
server needs `ariesd`.

| Binary | Runs on | Installed by |
|---|---|---|
| `aries` | Linux, macOS and Windows, on amd64 and arm64 | each person, on each machine where they or their agents work |
| `ariesd` | Linux, on amd64 and arm64 | the server's admin ([Running a server](server.md#3-install-ariesd)), or the server installer, below |

## Install aries

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh
```

The script installs the latest `aries` into `~/.local/bin` and prints the
line to add if that directory is not on your `PATH`. It never uses sudo or
edits your shell files. Before it installs anything, it checks:

- the release's Ed25519 signature, with OpenSSL 3 (without it, the script
  warns and checks only the sha256);
- the archive's sha256.

Options go after `sh -s --`, for example
`curl -fsSL … | sh -s -- --version v0.2.0`:

| Option | Variable | Does |
|---|---|---|
| `--version vX.Y.Z` | `ARIES_VERSION` | Install that release instead of the latest |
| `--dir DIR` | `ARIES_DIR` | Install into DIR instead of `~/.local/bin` |
| `--server` | | Install `ariesd` instead (Linux only) |
| `--require-signature` | | Fail, rather than warn, when OpenSSL cannot check the signature |
| | `ARIES_RELEASES` | Read releases from a mirror instead of the default ([below](#where-releases-are-read-from)) |

On Windows, download the zip from the
[releases page](https://github.com/ariesworx/aries/releases) and put
`aries.exe` on your `PATH`, or run the script inside WSL.

**First run.** `aries` needs no configuration of its own beyond the
repository's `.aries.yaml`. It signs in with the SSH key `ssh` would
use: a default file in `~/.ssh` (`%USERPROFILE%\.ssh` on Windows), such
as `id_ed25519`, and then the keys in ssh-agent. Send the admin that
key's `.pub`, then run `aries whoami` in the repository: it prints your
principal and which key signed in. To sign in with another key, set
`ARIES_KEY_FILE` to the file's path
([Your key is your identity](cli.md#your-key-is-your-identity)).

## Install a server

On a fresh Ubuntu 24.04 LTS or 26.04 LTS machine, `install-server.sh` sets up the
whole server, `ariesd` included, as root:

```sh
curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --dry-run
```

It installs `ariesd` and `aries` with `install.sh`, so they are checked
the same way, and reads `ARIES_RELEASES` or `--releases` for a mirror.
Drop `--dry-run` to set the server up.
[With the installer](server.md#with-the-installer) has its options, its
steps and what it leaves to you.

To have Claude Code do this from your workstation, and run the server
afterwards, with every change shown to you before it runs, install the
`aries-ops` plugin from this repository
([Operate a server with Claude Code](server.md#operate-a-server-with-claude-code)).

## Download by hand

Each [release](https://github.com/ariesworx/aries/releases) carries:

- `aries` for Linux, macOS and Windows, and `ariesd` for Linux, on amd64
  and arm64, as `<binary>_<version>_<os>_<arch>.tar.gz` (`.zip` on
  Windows);
- `checksums.txt`, the sha256 of every archive;
- `checksums.txt.sig`, the Ed25519 signature of `checksums.txt`.

Unpack the binary onto your `PATH`, after you
[verify](#verify-a-download-by-hand) the download.

## Where releases are read from

`install.sh`, `aries upgrade` and `ariesd upgrade` read releases from
GitHub unless told otherwise. One environment variable,
`ARIES_RELEASES`, tells all three:

| `ARIES_RELEASES` | Reads |
|---|---|
| unset | The default built into the binary or the script: GitHub today |
| `github` | GitHub |
| `https://releases.example.com/aries` | A static tree on a host that serves files over HTTPS |
| `/srv/aries-releases`, or `file:///srv/aries-releases` | The same tree in a directory: an absolute path |

A static tree is a file `index.json` and one directory for each release,
named by its tag, holding the files listed under
[Download by hand](#download-by-hand)
([RELEASING.md](../RELEASING.md#publishing-to-a-static-host) has the
layout). On a server, the config file's `releases:` does the same for
`ariesd upgrade` ([Upgrades](server.md#upgrades)).

**A mirror is not trusted, and needs no key.** Whatever the source, a
release is installed only when the signature on its `checksums.txt`
verifies against the release key built into the binary or the script,
and the archive's sha256 is in that file. A mirror that serves anything
else is refused. Over HTTP the address must be `https://`; plain `http://`
is accepted only to `127.0.0.1` or `[::1]`. No redirect is followed, and
the address may hold no user name, password or query.

What a mirror, like any source, can do is withhold: one that goes on
serving an old `index.json` keeps `aries upgrade` saying `up to date`.
`aries` never installs a release older than the one it is. `install.sh`
has nothing to compare with and installs the newest release the index
lists, so pass `--version` when you know which you want.

**Without a network.** Carry a release's files in on any medium: the
archives you need, `checksums.txt` and `checksums.txt.sig`. Put them in
a directory named by the tag, and name its parent:

```sh
mkdir -p /srv/aries-releases/v0.6.0
cp aries_0.6.0_linux_amd64.tar.gz checksums.txt checksums.txt.sig /srv/aries-releases/v0.6.0/
ARIES_RELEASES=/srv/aries-releases sh install.sh --version v0.6.0
```

A release named by `--version` needs no `index.json`. `aries upgrade`
looks for the latest release and so needs one; from a checkout,
`go run ./internal/tools/releaseindex v0.6.0 DIR /srv/aries-releases`
copies the release's files from DIR and writes it. To serve one mirror
to many machines, put the directory behind any HTTPS file server and
set `ARIES_RELEASES` to its URL.

## Build from source

```sh
go install github.com/ariesworx/aries/cmd/aries@latest
go install github.com/ariesworx/aries/cmd/ariesd@latest
```

Each builds the same code, unsigned, into `$(go env GOBIN)` or
`$(go env GOPATH)/bin`. Under a version manager such as mise, that is the
manager's own directory, which changes with the Go version. Prefer the
script. On a server, put `ariesd` where
[step 3](server.md#3-install-ariesd) installs it.

## Verify a download by hand

Work from a checkout of the release's tag, so the public key comes from the
repository rather than from the download. For release v0.2.0 on Linux:

```sh
git clone --branch v0.2.0 https://github.com/ariesworx/aries
cd aries
gh release download v0.2.0 -p checksums.txt -p checksums.txt.sig -p 'aries_0.2.0_linux_amd64.tar.gz'
go run ./internal/tools/releasekey verify checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify aries_0.2.0_linux_amd64.tar.gz --repo ariesworx/aries
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
| `aries upgrade --check` | Print one line: your version, the latest, and whether to upgrade. Changes nothing |
| `aries upgrade` | Replace `aries` with the latest release, after checking its signature and checksum |
| `aries upgrade --rollback` | Restore the binary the last upgrade replaced |

`aries upgrade` never runs by itself. When the server runs a newer version
than your `aries`, each command prints a one-line notice, and `prime`, which
starts an agent's session, includes it. To upgrade the server, see
[Upgrades](server.md#upgrades).

`aries upgrade` reads the release from GitHub, or from the mirror
`ARIES_RELEASES` names ([Where releases are read
from](#where-releases-are-read-from)), and asks your Aries server
nothing, so it works when the server refuses your `aries` as too old.

**When the server is newer, or older.** A release that changes the
protocol keeps accepting the one before it, so the server is upgraded
first and each `aries` after, at its own pace. An `aries` the server no
longer accepts, or one newer than the server, is refused at the
handshake with exit code 3 and a `fix:` line naming which side to
upgrade. Run `aries upgrade` on each machine, then restart your agents:
the MCP server an agent's harness started is the old `aries` until the
harness starts it again. If your `aries` is the newer one,
`aries upgrade --rollback` puts your previous `aries` back until the
server is upgraded.

**Coming from v0.4.x or earlier.** An upgrade in place is not possible.
A v0.4.x `aries` looks for releases under the project's former names
and trusts only the former release key, so `aries upgrade` finds
nothing it would install, and a v1.0.0 server refuses it anyway.
Install `aries` again with the script above, and follow
[Upgrading to 1.0](upgrade-to-1.0.md).

## Uninstall

1. In each repository you set up, run `aries setup --all --remove`. Add
   `--global` if you set agents up for every repository, and run
   `aries setup claude-desktop --remove` if you registered Claude Desktop.
2. Delete `aries`, and the `aries.prev` that an upgrade leaves beside it, from
   `~/.local/bin` or wherever you installed it.
