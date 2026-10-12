# Releasing starfix

A release is a pushed `vMAJOR.MINOR.PATCH` tag. The `release` workflow does
the rest in three jobs, each waiting for the one before:

| Job | Secrets | Does |
|---|---|---|
| `gate` | none | Runs the full CI gate (`ci.yml`, called as a reusable workflow) on the tagged commit: lint, `go mod verify`, govulncheck, the cross-platform builds, and every test with `-race -shuffle=on` against the pinned Dolt, which must be present (`STARFIX_REQUIRE_DOLT=1`). A tag whose commit fails it is never built or signed |
| `build` | none | Downloads and verifies the modules (`go mod verify`) into a clean cache and runs `govulncheck`, failing on a known vulnerability; builds `sfx` for linux, darwin and windows on amd64 and arm64, and `starfixd` for linux on amd64 and arm64 (`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X …version.Version=<tag>"`); packs each into `<bin>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) with the LICENSE and README; writes `checksums.txt` (sha256); attests build provenance for every archive; uploads them as a workflow artifact |
| `sign-and-publish` | `STARFIX_RELEASE_KEY`, in the `release` environment | Waits for the maintainer's approval; checks the archives against `checksums.txt`; signs it into `checksums.txt.sig` with `internal/tools/releasekey`; creates the GitHub release with every file |

`checksums.txt.sig` is one line: the standard base64 of a raw Ed25519
signature over the exact bytes of `checksums.txt`. `sfx upgrade` and
`starfixd upgrade` accept a release only when that signature verifies
against a key in `internal/release/keys.go` and the archive's sha256 is in
`checksums.txt`. The signing step refuses a key that `keys.go` at the tag
does not list, so a release clients would refuse is never published.

The release notes name the Dolt version the release was tested with
(`version.Dolt`, which a test keeps equal to CI's `DOLT_VERSION`).

They also say, first, when the release changes the protocol
(`internal/proto/version.go`): the version it speaks and the range it
accepts, and which earlier releases keep working. Normally all of the
one before do, since `ProtoMin` stays one version behind. **The first
release after v0.4.0 is the exception:** it speaks and accepts protocol
5 alone, so every earlier `sfx` is refused by it, and it is refused by
every earlier server. Its notes must lead with that, give the order
(server, then every `sfx`, then restart the agents) and link
[Upgrades](docs/server.md#upgrades), which has the text each side
prints.

**The same release changes which key `sfx` signs in with, and its notes
must say so beside the protocol.** A key named for `sfx` is now the only
key it offers the server, and `key:` in `.starfix.yaml` may name only a
file inside the repository. A `.starfix.yaml` with a line such as `key:
~/.ssh/id_ed25519`, which every earlier release accepted, is refused:
`sfx` says so, names the line to remove, and gives the replacement, the
environment variable `STARFIX_KEY_FILE=~/.ssh/id_ed25519`, which holds
the key file's path. The notes give that one step and three more
things. With no key named, `sfx` now tries the default identity files of
`~/.ssh` before ssh-agent's keys, as `ssh` does, and offers five keys at
most, so a person whose key is `~/.ssh/id_ed25519` needs no ssh-agent,
and one with several registered keys may sign in by a different one
than before. A CI job that followed the team guide's earlier recipe has
a secret named `STARFIX_KEY` holding a private key: `sfx` never reads
it, and the notes link the new recipe
([docs/deploy/team.md](docs/deploy/team.md)), which writes the key to a
file and sets `STARFIX_KEY_FILE` to its path. And `sfx whoami` shows
which principal and key a connection used. The notes link [Your key is your
identity](docs/cli.md#your-key-is-your-identity).

**That same release is the first whose starfixd refuses to start with
its clock behind its own history** (`clock_behind: stop`, design §14,
decision 37). Its notes must say so under the protocol change: a server
whose clock once ran ahead holds events dated in the future and will not
start after the upgrade; how to find out beforehand (the query in
[Upgrades](docs/server.md#upgrades)); the three ways on (wait, set
`clock_behind: warn`, or start once with `--accept-clock-behind`); the
unit lines to add (`RestartPreventExitStatus=75`, `time-sync.target`);
and that a usage record may now be 5 minutes past the server's clock,
not an hour. The upgrade from v0.4.0 cannot ask the question itself,
since the binary that runs it predates the check: it puts the old
binary back and reports the new one unhealthy.

## Changing the Dolt pin

The Dolt release is pinned in five files, and one pull request changes
them all. Tests in `internal/version` fail until they agree:

| File | Change |
|---|---|
| `internal/version/version.go` | `Dolt` |
| `internal/version/dolt.sha256` | All four lines: the SHA-256 of the linux and darwin archives, amd64 and arm64, each named `vVERSION/ARCHIVE`. `scripts/install-dolt.sh` verifies a contributor's download against it |
| `.github/workflows/ci.yml` | `DOLT_VERSION`, and `DOLT_SHA256`, the linux-amd64 checksum |
| `deploy/docker/Dockerfile` | `DOLT_VERSION`, `DOLT_SHA256_AMD64` and `DOLT_SHA256_ARM64` |
| `docs/server.md` | The download in step 1, and the version under "What a server needs" |

GitHub records each archive's digest. This prints the four lines of
`dolt.sha256` for a release; put the new version in both places:

```sh
gh api repos/dolthub/dolt/releases/tags/v2.4.2 --jq '.assets[]
  | select(.name | test("^dolt-(linux|darwin)-(amd64|arm64)\\.tar\\.gz$"))
  | "\(.digest | ltrimstr("sha256:"))  v2.4.2/\(.name)"'
```

Then check the pins against each other and against a real download:

```sh
go test ./internal/version ./scripts
scripts/install-dolt.sh
```

The digests come from the same place as the archives, so the pin
protects against an archive that changes after it was pinned, not
against a release that was bad when it was pinned. Run the whole gate
against the new Dolt before merging: a Dolt release can change behavior
the store depends on (`spike/dolt/RESULTS.md`).

## Cutting a release

When to cut the first one is the maintainer's call.

1. Make sure the commit to tag is on `main` and its CI is green. The
   `gate` job runs the same checks again on the tag, so a red commit
   stops there, but finding out first saves a tag.
2. Tag and push; only the maintainer can (the tag ruleset below):

   ```sh
   git tag -s v0.3.0 -m v0.3.0
   git push origin v0.3.0
   ```

3. When `build` passes, approve the `sign-and-publish` deployment
   (Actions → the run → Review deployments → `release`).
4. Check the release page lists eight archives, `checksums.txt` and
   `checksums.txt.sig`, then on any machine: `sfx upgrade --check`.

A tag with a `-` (`v0.3.0-rc.1`) is published as a prerelease, which
`sfx upgrade` ignores; `starfixd upgrade --to v0.3.0-rc.1` installs it.

If a run fails after the release was created, delete the release (not the
tag) and re-run the workflow.

## One-time setup

The maintainer does these by hand. Replace `<maintainer>` with the
maintainer's GitHub login.

1. **Generate the key** on a trusted machine, from a checkout:

   ```sh
   go run ./internal/tools/releasekey gen release.key
   ```

   It writes the private key to `release.key` (mode 0600) and prints only
   the public key, as the line to add to `keys.go`.

2. **Create the `release` environment**: the maintainer as the only
   required reviewer, self-review allowed (he is the only reviewer), and
   only `v*` tags may deploy to it.

   ```sh
   id=$(gh api users/<maintainer> --jq .id)
   gh api -X PUT repos/ariesworx/starfix/environments/release --input - <<JSON
   {"reviewers": [{"type": "User", "id": $id}],
    "prevent_self_review": false,
    "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}
   JSON
   gh api -X POST repos/ariesworx/starfix/environments/release/deployment-branch-policies \
     -f name='v*' -f type=tag
   ```

3. **Store the key** as an environment secret, keep an offline backup
   (for example on encrypted removable media in a safe), then delete the
   local file:

   ```sh
   gh secret set STARFIX_RELEASE_KEY --env release < release.key
   # back release.key up offline, then:
   shred -u release.key   # rm -P on macOS
   ```

4. **Restrict `v*` tags** to the maintainer with a tag ruleset that blocks
   creating, updating and deleting them for everyone but repository
   admins (actor 5 is the built-in Admin role; keep the maintainer the
   only admin):

   ```sh
   gh api -X POST repos/ariesworx/starfix/rulesets --input - <<'JSON'
   {"name": "release tags", "target": "tag", "enforcement": "active",
    "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
    "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}],
    "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}]}
   JSON
   ```

   Check under Settings → Rules that the bypass list shows only Repository
   admin.

5. **Publish the public key**: paste the printed line into
   `internal/release/keys.go` by pull request. Until that merges, every
   build refuses every release (`no release signing key is configured`).

### What this protects, and what it does not

The key is usable only by a `release` run that the maintainer approved,
on a `v*` tag that only the maintainer can push. Nobody can read it back
out of GitHub through the UI or API. But GitHub itself, and owners of the
`ariesworx` organization (who can change environments, rulesets and
workflows), can still reach it. That is the accepted trade-off for
signing in CI rather than on the maintainer's machine. Build provenance
(`gh attestation verify`) is an independent check that an archive came
from this repository's workflow.

## Rotating the key

Clients trust whatever `keys.go` held when they were built, so a new key
must ship in a release before anything is signed with it.

1. `go run ./internal/tools/releasekey gen new.key`; add the new line to
   `keys.go` **beside** the old one, by pull request.
2. Release (signed with the old key). Every client that installs it now
   trusts both keys.
3. Replace the secret: `gh secret set STARFIX_RELEASE_KEY --env release < new.key`;
   back up `new.key` offline and delete it locally.
4. Release again (signed with the new key). Clients older than step 2
   refuse this release; they upgrade to the step-2 release first by hand.
   `install.sh` trusts one key, as a PEM: switch it to the new one
   (`go run ./internal/tools/releasekey` prints raw keys; the PEM is the
   base64 of `302a300506032b6570032100` and the 32 key bytes) in a pull
   request that merges as this release is published. A test keeps it in
   `keys.go`.
5. After a while, remove the old key from `keys.go` by pull request.

If the old key is compromised, skip the overlap: remove it from `keys.go`
and `install.sh` in the same pull request that adds the new one, replace the secret, and
release. Every client older than that release refuses it and has to be
reinstalled by hand (README, Install); say so in the release notes.
