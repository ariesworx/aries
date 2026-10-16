# Releasing Aries

A release is a pushed `vMAJOR.MINOR.PATCH` tag, and then the maintainer's
word. The `release` workflow does it in three jobs over two runs. The
pushed tag starts the first run, which is `gate` and then `build`, and
stops there. The maintainer starts the second by hand at the same tag,
naming the first, and it is `sign-and-publish`:

| Job | Secrets | Does |
|---|---|---|
| `gate` | none | Runs the full CI gate (`ci.yml`, called as a reusable workflow) on the tagged commit: lint, `go mod verify`, govulncheck, the cross-platform builds, and every test with `-race -shuffle=on` against Postgres at the pinned major version, which must be present (`ARIES_REQUIRE_POSTGRES=1`). It restores no Go cache: it downloads every module and compiles everything. A tag whose commit fails it is never built or signed |
| `build` | none | Downloads and verifies the modules (`go mod verify`) into a clean cache and runs `govulncheck`, failing on a known vulnerability; builds `aries` for linux, darwin and windows on amd64 and arm64, and `ariesd` for linux on amd64 and arm64 (`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X …version.Version=<tag>"`); packs each into `<bin>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) with the LICENSE and README; writes `checksums.txt` (sha256); attests build provenance for every archive; uploads them as a workflow artifact |
| `sign-and-publish` | `ARIES_RELEASE_KEY`, in the `release` environment | Runs only when started by hand (`workflow_dispatch`), and first refuses anyone but the release approver, any ref but a release tag, and any build run that is not this workflow's own for this tag and commit, and green. Then it downloads that run's archives; checks them against `checksums.txt`; signs it into `checksums.txt.sig` with `internal/tools/releasekey`; creates the GitHub release with every file |

**Starting the second run is the approval.** It was once a required
reviewer on the `release` environment. GitHub enforces required
reviewers on a private repository only for its Enterprise plan, so on any
other plan a private repository would sign whatever tag was pushed. A
run that one named person must start holds on every plan and for a
public repository alike. A test (`internal/release/workflow_test.go`)
pins the checks that job makes before it reads the key.

`checksums.txt.sig` is one line: the standard base64 of a raw Ed25519
signature over the exact bytes of `checksums.txt`. `aries upgrade` and
`ariesd upgrade` accept a release only when that signature verifies
against a key in `internal/release/keys.go` and the archive's sha256 is in
`checksums.txt`. The signing step refuses a key that `keys.go` at the tag
does not list, so a release clients would refuse is never published.

That check is all a release is trusted on. Where its files are read from
is not: GitHub today, and a static file host once the repository is
private ([Publishing to a static host](#publishing-to-a-static-host),
[Before the repository is made
private](#before-the-repository-is-made-private)).

The release notes name the Postgres release the tests pin
(`version.Postgres`), which the workflow reads from `version.go`; a
test keeps the two in step. CI ran the gate on the newest release of
that major version.

They also say, first, when the release changes the protocol
(`internal/proto/version.go`): the version it speaks and the range it
accepts, and which earlier releases keep working. From v1.0.0 on, the
one before always does, since a bump keeps `ProtoMin` at the version
before (rule 10). **v1.0.0 is the exception:** under decisions 78 and
88 its protocol is settled at 8, spoken alone, and it refuses every
client and server of v0.4.x and earlier, and every build of 5 to 7,
which no release spoke. Its notes
([docs/releases/v1.0.0.md](docs/releases/v1.0.0.md)) lead with that and
link [Upgrading to 1.0](docs/upgrade-to-1.0.md).

What this section once required of the first release after v0.4.0
(the key `aries` signs in with, and the refusal to start with the clock
behind) concerned upgrading a v0.4.0 server in place, which v1.0.0 does
not allow; its notes give both as breaking changes.

## Changing the Postgres pin

Postgres is pinned twice: the major version the tests run on
(`PostgresMajor`), which CI installs from the PostgreSQL project's apt
repository at its newest release, and the release that
`scripts/install-postgres.sh` builds where no package of that major
version is installed (`Postgres`, with the SHA-256 of its source
archive). `PostgresMin`, the oldest release the store opens, is not a
pin: it changes by decision, and the store's message names it. One pull
request changes these files, and tests in `internal/version` fail until
they agree:

| File | Change |
|---|---|
| `internal/version/version.go` | `Postgres`, and `PostgresMajor` when the major version changes |
| `internal/version/postgres.sha256` | The one line: the SHA-256 of the source archive, named `vVERSION/postgresql-VERSION.tar.gz` |
| `.github/workflows/ci.yml` | `POSTGRES_MAJOR`, when the major version changes |
| `CONTRIBUTING.md` | The release `scripts/install-postgres.sh` builds, and the major version under "What you need" |
| `install-server.sh` | `pg_major`, the package the installer installs from the PostgreSQL project's apt repository, when the major version changes; and `pg_min` with `PostgresMin` |
| `docs/server.md` | Step 1's `apt-get install -y postgresql-MAJOR`, and the cluster's name (`postgresql@MAJOR-main`) where the guide gives it, when the major version changes |
| `deploy/docker/compose.yaml` | The `postgres` image: its digest, from Docker's official image of the new release, and the comment above it that names the release |

The PostgreSQL project publishes each archive's SHA-256 beside it, as
`postgresql-VERSION.tar.gz.sha256` in the release's directory of
<https://ftp.postgresql.org/pub/source/>. Put it in `postgres.sha256`,
then check the pins against each other and against a real build, which
`install-postgres.sh` makes under a new directory of the cache for the
new release:

```sh
go test ./internal/version ./scripts
scripts/install-postgres.sh
docker buildx imagetools inspect postgres:18
```

The last command prints the official image's index digest, which the
compose file pins (the line `Digest:`); its annotations name the release
the image holds.

The digest comes from the same place as the archive, so the pin protects
against an archive that changes after it was pinned, not against a
release that was bad when it was pinned. Run the whole gate against the
new release before merging: a new major version can change what the
store depends on, such as which SQLSTATE a lost race reports.

Tests in `scripts` keep the installer's `pg_major` and `pg_min`, the
compose file's image and the guide's step 1 at `PostgresMajor` and
`PostgresMin`, and the installer's key fingerprint at CI's
`PGDG_KEY_FINGERPRINT`. Run the [Docker smoke
test](docs/deploy/docker.md#run-the-smoke-test) against the new image.

## Cutting a release

When to cut the first one is the maintainer's call.

1. Update the release notes, `docs/releases/vX.Y.Z.md`
   ([v1.0.0's](docs/releases/v1.0.0.md) is the model), by pull request:
   what is new, breaking changes, security changes, removals, the
   supported platforms, and what was not verified. Nothing may still be
   marked **(planned)**: an item has landed, or it leaves the notes. A
   release that needs more than `aries upgrade` and `ariesd upgrade`
   also gets an upgrade guide beside them, as
   [Upgrading to 1.0](docs/upgrade-to-1.0.md) is for v1.0.0; take its
   draft mark off too.
2. Make sure the commit to tag is on `main` and its CI is green. The
   `gate` job runs the same checks again on the tag, so a red commit
   stops there, but finding out first saves a tag.
3. Tag and push; only the maintainer can (the tag ruleset below):

   ```sh
   git tag -s v0.3.0 -m v0.3.0
   git push origin v0.3.0
   ```

4. When `build` passes, its run's summary gives the command that signs
   and publishes; run it. It names the tag and that run:

   ```sh
   gh workflow run release.yml --ref v0.3.0 -f build_run=<the build run's id>
   ```

   Nobody else's run of it is accepted, and neither is a build run of
   another tag or commit, or one that failed.
5. Check the release page lists eight archives, `checksums.txt` and
   `checksums.txt.sig`, then on any machine: `aries upgrade --check`.
6. Put the release notes on the release page; the workflow writes only
   a line and GitHub's generated list:

   ```sh
   gh release edit v0.3.0 --notes-file docs/releases/v0.3.0.md
   ```

A tag with a `-` (`v0.3.0-rc.1`) is published as a prerelease, which
`aries upgrade` ignores; `ariesd upgrade --to v0.3.0-rc.1` installs it.

If the signing run fails after the release was created, delete the
release (not the tag) and start the signing run again with the same
`build_run`. The archives are kept with the build run for 7 days; after
that, re-run the build run first and sign the new one.

## Publishing to a static host

`aries upgrade`, `ariesd upgrade` and `install.sh` can read releases from
any host that serves files over HTTPS, such as a storage bucket: no API,
no listing, no credentials. The release workflow does not publish there
yet; that step is the maintainer's to add. This section is what the
clients read, and the tool that writes it.

**The layout**, under a base URL `B`
(`https://releases.example.com/aries`):

| Path | Holds |
|---|---|
| `B/index.json` | The releases the tree holds |
| `B/<tag>/checksums.txt` | The sha256 of every archive of release `<tag>` (`v0.6.0`) |
| `B/<tag>/checksums.txt.sig` | Its signature |
| `B/<tag>/<bin>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) | The archives, under the names the `build` job gives them |

**`index.json`** is written one release to a line, oldest first:

```json
{
  "format": 1,
  "releases": [
    {"tag": "v0.5.0", "prerelease": false, "published": "2026-09-01T12:00:00Z"},
    {"tag": "v0.6.0-rc.1", "prerelease": true, "published": "2026-09-20T12:00:00Z"},
    {"tag": "v0.6.0", "prerelease": false, "published": "2026-10-01T12:00:00Z"}
  ]
}
```

| Field | Is |
|---|---|
| `format` | `1`. The format only gains fields, which older builds ignore. Change the number only when older builds must stop reading the file: they refuse any other |
| `tag` | The release's tag and its directory: `vMAJOR.MINOR.PATCH`, with the optional `-prerelease` |
| `prerelease` | Whether the tag has a `-`. It repeats the tag for readers that do not parse versions |
| `published` | When the release was added, in UTC as RFC 3339. For people; no build reads it |

Nothing in the index is trusted. A client uses it for one thing, to
choose the tag to ask for, and then checks the release behind the tag as
it checks any other. What it does with the file:

- The latest release is the highest version that is not a prerelease,
  by version number: not the last line, and not the newest `published`.
- `aries` and `ariesd` refuse the whole file when it is larger than
  1 MiB, lists more than 4,096 releases, is another `format`, names a
  tag that is not a release version, or gives a `prerelease` that its tag
  contradicts. A tag is checked before it is put in a URL, so one such
  as `../x` or a URL never is. `install.sh` refuses a file over 1 MiB or
  with a tag that is not a release version, and never prints the tag.
- `ariesd upgrade --to` and `install.sh --version` do not read the
  index at all.
- No redirect is followed, so the host named is the only one asked, and
  over HTTPS only. Plain HTTP is accepted only to a loopback address.
  A host that moves must go on serving the old address; it cannot send
  clients on.

**What the host can and cannot do.** It cannot get a client to install
anything the release key did not sign, or a release other than the one
the client asked for: `checksums.txt` names each archive with its
version, so one release's signed files do not pass for another's. It
cannot move a server or an `aries` back to an older release: neither
installs a version older than the one running. It can refuse to serve,
and it can go on serving an old `index.json`, which keeps every client
on the release it has and silent about newer ones. Nothing a client
reads proves an index is fresh, and that is as true of GitHub as of a
bucket: say so when a security release goes out, through a channel that
is not the release host. `install.sh` has no version to compare with, so
an old index gives a new machine an old, signed release.

**The tool.** `internal/tools/releaseindex` turns the directory the
`sign-and-publish` job ends with (the archives, `checksums.txt` and
`checksums.txt.sig`) into that layout:

```sh
go run ./internal/tools/releaseindex --published 2026-10-01T12:00:00Z v0.6.0 dist/ tree/
```

It copies the files to `tree/v0.6.0/` and adds the release to
`tree/index.json`, which it writes the same, byte for byte, for the same
releases. Without `--published` a release keeps the time the index
already gives it, or takes the present time when it is new. The tool
signs nothing and needs no secret. Before it writes anything it checks
what a client would: the signature on `checksums.txt`, against the
public keys in `keys.go`, and each archive's sha256. It refuses a
`dist/` whose files are another release's, and a `tree/` that already
holds the release with different bytes, since a published release does
not change. It is Go and not a shell script because it reads, orders
and rewrites JSON, and because it then uses the same parser, the same
bounds and the same signature check as the clients.

`tree/` is then what the host serves. To publish, start from the host's
current `index.json`, so that the releases already there stay listed,
and upload the index last, so that it never names files that are not
there yet:

```sh
mkdir -p tree && curl -fsS -o tree/index.json https://releases.example.com/aries/index.json
go run ./internal/tools/releaseindex v0.6.0 dist/ tree/
# copy tree/v0.6.0/ to the host, then tree/index.json, with the host's own tool
```

Serve `index.json` with a short cache lifetime (`Cache-Control: max-age=300`
or `no-cache`): a cached copy hides a new release for as long as it
lives. Everything under `<tag>/` may be cached forever.

**A mirror.** The same layout in a directory or behind another HTTPS
host is a mirror, and `ARIES_RELEASES` points a client at one
([Install](docs/install.md#where-releases-are-read-from),
[Upgrades](docs/server.md#upgrades)). Build one with the same command,
from a release's files carried in on any medium. A mirror is trusted
with nothing more than the bucket is.

## Before the repository is made private

`aries upgrade`, `ariesd upgrade` and `install.sh` read GitHub without
credentials. The day the repository is private, each of those requests
fails, on every machine. The order below is the only one that leaves no
client stranded, and its last step waits for every step before it.

1. **Publish to the bucket.** Lay out every release clients may still
   need, the current one at least, and serve it
   ([Publishing to a static host](#publishing-to-a-static-host)). Add the
   upload to the release workflow. Check it from outside:
   `ARIES_RELEASES=https://releases.example.com/aries aries upgrade --check`.
2. **Switch the default, in a release.** One pull request changes
   `DefaultSource` in `internal/release/static.go` to the bucket's URL
   and `DEFAULT_RELEASES` in `install.sh` to the same; `TestDefaultSource`
   fails until they agree. Publish that release to GitHub and to the
   bucket: the clients that must install it still read GitHub.
3. **Every client and every server upgrades to that release.** `aries
   upgrade` on each machine, `ariesd upgrade` on each server. From
   then on they read the bucket.
4. **Only then change the repository's visibility.**

**A client or server older than the release of step 2 cannot upgrade
itself once the repository is private.** It asks GitHub, is refused, and
says it cannot read the release. It is repaired by hand, in one of two
ways: install again with the `install.sh` of step 2 or later, or run the
old binary once with `ARIES_RELEASES` set to the bucket's URL, if it is
recent enough to read that variable (every release from the one that
introduced the static source is). A release older than that knows only
GitHub and must be reinstalled.

Before step 4, also move what else reads the public repository, none of
which this change covers:

- **`install.sh` itself.** The one-line install fetches it from
  `raw.githubusercontent.com`, which stops with the repository. Serve it
  from the bucket and change the command in `README.md` and
  `docs/install.md`.
- **Words that name GitHub.** Several `fix:` lines of `aries upgrade` and
  `ariesd upgrade` send people to the releases page
  (`releasesURL` in `internal/cli/upgrade.go` and
  `cmd/ariesd/upgrade.go`), and `install.sh` names it for Windows and
  for reports. `go install …@latest` and `git clone`
  ([Build from source](docs/install.md#build-from-source)) stop working
  for anyone without access.
- **Verifying by hand.** `gh release download` and
  `gh attestation verify --repo` need access to the repository
  ([Verify a download by hand](docs/install.md#verify-a-download-by-hand)).
  The signature check does not: it needs the public key, which is in
  every binary and in `install.sh`.

## One-time setup

The maintainer does these by hand. Replace `<maintainer>` with the
maintainer's GitHub login.

1. **Generate the key** on a trusted machine, from a checkout:

   ```sh
   go run ./internal/tools/releasekey gen release.key
   ```

   It writes the private key to `release.key` (mode 0600) and prints only
   the public key, as the line to add to `keys.go`.

2. **Create the `release` environment**: only `v*` tags may deploy to
   it, and it names the release approver, the one login whose run may
   sign.

   ```sh
   gh api -X PUT repos/ariesworx/aries/environments/release --input - <<'JSON'
   {"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}
   JSON
   gh api -X POST repos/ariesworx/aries/environments/release/deployment-branch-policies \
     -f name='v*' -f type=tag
   gh variable set RELEASE_APPROVER --env release --body '<maintainer>'
   ```

   Without `RELEASE_APPROVER` the signing run refuses to start its work
   and says so. Changing the variable, like the environment itself,
   takes admin rights on the repository.

   On a public repository, or on GitHub's Enterprise plan, a required
   reviewer on the environment may be kept as well: the maintainer then
   approves the run they started. It is not what the release relies on.

3. **Store the key** as an environment secret, keep an offline backup
   (for example on encrypted removable media in a safe), then delete the
   local file:

   ```sh
   gh secret set ARIES_RELEASE_KEY --env release < release.key
   # back release.key up offline, then:
   shred -u release.key   # rm -P on macOS
   ```

4. **Restrict `v*` tags** to the maintainer with a tag ruleset that blocks
   creating, updating and deleting them for everyone but repository
   admins (actor 5 is the built-in Admin role; keep the maintainer the
   only admin):

   ```sh
   gh api -X POST repos/ariesworx/aries/rulesets --input - <<'JSON'
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

The key is usable only by a `release` run that the release approver
started, on a `v*` tag that only the maintainer can push. Three things
hold that, and each needs the others: the tag ruleset, so that the
workflow file at a `v*` tag is always one the maintainer tagged; the
environment's branch policy, so that the key is offered to no other ref;
and the first step of `sign-and-publish`, so that another person with
write access cannot start the maintainer's own workflow at the
maintainer's own tag. The last is a check inside the workflow, not one
GitHub makes, which is why the first two matter: nobody else can put a
different workflow where the key can be read. Nobody can read it back
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
3. Replace the secret: `gh secret set ARIES_RELEASE_KEY --env release < new.key`;
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
