# Packaging: an icon, and installers for macOS and Windows

**Design, 23 October 2026 (aries-j3x2mda6); proposed.** Nothing in it is
built, and everything it proposes for the release workflow, signing or
a certificate is the maintainer's to decide, add and merge (AGENTS.md,
rules 3 and 4). §8 has the questions only the maintainer can answer.

## 1. Why

The maintainer, 23 October 2026: "Can we build an icon for aries that
people can see or install via .dmg or .exe as well?"

Today each release carries `aries` for Linux, macOS and Windows and
`ariesd` for Linux, as `.tar.gz` and `.zip` archives, with
`checksums.txt` and its Ed25519 signature (`release.yml`,
[install.md](../install.md#download-by-hand)). `install.sh` installs
`aries` into `~/.local/bin` without sudo, and `aries upgrade` swaps the
running binary in place after checking the signature
(`internal/release/swap.go`). A Windows user downloads the zip and puts
`aries.exe` on `PATH` by hand. There is no icon, no `.dmg`, `.pkg`,
`.msi` or `.exe` installer, and no Apple or Authenticode signature.

Two things are being asked, and they are separable:

- **An icon**, so that Aries is recognizable where an icon is shown.
- **An installer a person double-clicks**, on macOS and on Windows,
  that puts `aries` on `PATH`.

The second is where the cost is: on both systems an installer that is
not signed by a certificate the system trusts is met with a warning
that most people read as "this is malware" (§3.1, §4.1). An unsigned
installer is worse than none, because the zip and `install.sh` raise
no such warning today on the paths people use (§3.1).

## 2. The icon

### 2.1 Where it would be seen

| Place | Shows an icon? | Needs |
|---|---|---|
| A `.dmg`'s volume and window | Yes: the mounted volume's icon (`.VolumeIcon.icns`) and a background image | `.icns`, a PNG background |
| A macOS `.pkg` | Only inside Installer's window, as a background or logo; the `.pkg` file itself shows Installer's generic icon once downloaded | a PNG |
| A Windows installer and Add/Remove Programs | Yes: the installer `.exe`'s own icon, and the entry in Settings, Apps (`ARPPRODUCTICON` in an `.msi`) | `.ico` |
| `aries.exe` in Explorer and the taskbar | Only if the icon is embedded as a Windows resource at build time (a `.syso` beside `cmd/aries`); a Go binary has none by default | `.ico`, and a tool to write the `.syso` (§5.2) |
| The Claude Desktop extension (identity.md §9, slice N7) | Yes: an MCP bundle's `manifest.json` names an icon | a PNG |
| The README, the docs, the GitHub repository's social preview | Yes | the SVG, a PNG |
| `aries` and `ariesd` on Linux, `aries` run in a terminal, `aries tui` | No. A terminal's title is text (OSC 0 and 2 set a string, not an image), and a CLI binary on macOS or Linux carries no icon a shell shows | nothing |

So the icon matters for exactly the installers asked about, the desktop
extension that identity.md already plans, and the project's pages. It
does nothing for `aries tui` or for people who install with
`install.sh`.

### 2.2 One source, the rest built from it

- **The source is one SVG**, `assets/icon/aries.svg`, drawn on a square
  canvas, legible at 16 px (a single bold mark with no text and no fine
  lines), with a variant for dark backgrounds only if the mark needs
  one. Text in an icon does not survive 16 px and would have to be
  translated.
- **PNGs at 16, 32, 48, 64, 128, 256, 512 and 1024 px are rendered from
  it and committed beside it.** Rasterizing SVG is not in Go's standard
  library, and an SVG renderer is a dependency we would rather not take
  into the release (rule 4). The person who changes the SVG renders the
  PNGs once with any tool (Inkscape, `rsvg-convert`) and commits them;
  a small-size PNG may be hand-tuned, which is the usual practice for
  16 and 32 px anyway.
- **`.ico` and `.icns` are packed from the PNGs by a tool in this
  repository**, `internal/tools/icons`, on the standard library alone:
  both formats are a short header and a table of entries, and both
  accept PNG data for each size (ICO since Windows Vista; ICNS's
  `ic07` to `ic10` types). No new dependency, and it runs on Linux, so
  the build job need not be on macOS for this. A test holds the
  committed `.ico` and `.icns` to what the tool makes of the PNGs, so
  they cannot drift; `iconutil` on macOS can check the `.icns` by hand.

### 2.3 Who draws it

A person, not an agent. The icon is the project's identity, it outlives
any one release, and it may be registered as a mark; an agent can draft
a placeholder SVG so that the packaging work is not blocked, but that
placeholder ships to nobody. The choices are the maintainer's (§8,
questions 1 and 2): draw it, commission a designer (with the rights
assigned to the copyright holder, so it can be licensed with the
project), or hold the installers until there is one.

Rule 1 applies to the artwork: the SVG names no organization, person
or host, and carries no wordmark. Its license is a question (§8, 2):
under AGPL-3.0-only with the rest, or kept out of the code's license as
a trademark is, so that a fork can be told to use its own.

## 3. macOS

### 3.1 What Gatekeeper does today, and would do

Gatekeeper checks a file that carries the quarantine attribute
(`com.apple.quarantine`), which a browser, Mail, AirDrop and most
download tools set, and `curl` does not. So:

- **`install.sh` is not affected**: it fetches with `curl`, and the
  binary it installs carries no quarantine attribute. Neither is
  `aries upgrade`, which writes the file itself, nor `go install`.
- **A `.tar.gz` downloaded in a browser** is quarantined, and Archive
  Utility passes the attribute to `aries` when it unpacks it. Run from a
  terminal, an unsigned, un-notarized `aries` is refused. Go's linker
  signs every `darwin/arm64` binary ad hoc, because Apple silicon runs
  nothing unsigned, but an ad hoc signature is not a Developer ID and
  does not satisfy Gatekeeper. A person who knows to can run
  `xattr -d com.apple.quarantine aries`; most will not know to.
- **An unsigned `.pkg` or `.dmg` downloaded in a browser** is refused
  when opened. Since macOS 15 the Control-click, Open override is gone:
  the person opens System Settings, Privacy & Security, and chooses
  Open Anyway beside a notice that the file "was blocked to protect
  your Mac" (Apple's announcement and press coverage, Read; reports
  that 15.1 removed even that are **Not confirmed**).

So an unsigned installer is a regression: it adds a warning on the
very path, double-clicking a download, that it exists to make easy.
**An installer for macOS is worth building only signed and
notarized.**

### 3.2 What signing and notarization need

- **Membership of the Apple Developer Program**, as an organization:
  99 USD a year (Apple's fee, as known in 2026; to be checked on
  enrollment), a D-U-N-S number for the legal entity, and an Account
  Holder who is a person at it.
- **Two certificates** from that account: *Developer ID Application*,
  which signs `aries` itself (`codesign --options runtime --timestamp`;
  notarization requires the hardened runtime and a secure timestamp),
  and *Developer ID Installer*, which signs a `.pkg` (`productsign`, or
  `productbuild --sign`). Each is a private key and its certificate,
  exported as a `.p12` with a password.
- **Credentials for the notary service**: an App Store Connect API key
  (its issuer id, key id and `.p8` file) for `xcrun notarytool submit
  --wait`, preferred over an Apple ID with an app-specific password
  because it is not a person's login and can be revoked alone.
  `xcrun stapler staple` then attaches the ticket to a `.pkg` or
  `.dmg`, so it opens without a network; a bare Mach-O binary cannot be
  stapled, and Gatekeeper looks its ticket up online.
- **Where they live**: all three are new secrets of the gated `release`
  environment, created and stored by the maintainer, read only by the
  run the maintainer starts (rule 3; the same rule identity.md §9 gives
  the desktop extension's certificate). No agent creates, sees or
  handles them, and the steps that use them are the maintainer's to
  add.
- **Where the tools run**: `codesign`, `productbuild`, `notarytool`
  and `stapler` are Xcode's and run on a macOS runner (`macos-15`,
  with the Xcode version selected by path, not left to the image). The
  alternative is `rcodesign` (the `apple-codesign` project, MPL-2.0,
  Read), which signs and notarizes from Linux; it is one more tool to
  admit and pin, and keeps the job on Ubuntu (§5.2).

### 3.3 `.pkg` or `.dmg`

`aries` is a CLI. A `.dmg` is the convention for an *app*: a window
with the app and an alias to `/Applications` to drag it onto. There is
no drag target that puts a binary on `PATH`, so a `.dmg` of `aries`
would hold the binary and a README telling the person to copy it, which
is the zip with extra steps, or would hold a `.pkg`.

**A `.pkg` fits**: a flat package, built by `pkgbuild` and
`productbuild` with a distribution file, that installs
`/usr/local/bin/aries` (on the default `PATH` through `/etc/paths`, on
Intel and Apple silicon alike), with the license page, the icon as
Installer's background, and a universal binary (`lipo` of the `amd64`
and `arm64` builds) so one file serves both. It is also what an
organization's device management (Jamf, Intune, Kandji) deploys: they
take a signed `.pkg`, not a `.dmg`.

**The `.dmg` is then optional**: a signed, notarized, stapled `.dmg`
holding the `.pkg`, with the icon as its volume icon, if the maintainer
wants the volume to show it (§8, 4). Making one needs `hdiutil`, which
is macOS-only.

### 3.4 What a `.pkg` changes for `aries upgrade`

A `.pkg` installs as root: `/usr/local/bin/aries` is root's, and
`aries upgrade`, which writes the new binary beside the old
(`Swap.stage`, `keepPrevious`), fails for the person running it. And
the package database (`pkgutil --pkgs`) would go on naming the old
version. So `aries upgrade` should notice a binary it did not install
and say what to do, a refusal with a fix (rule 11): "installed by the
macOS package; download the new `.pkg`, or run `sudo aries upgrade`".
It can tell by the path, or by a receipt (`pkgutil --pkg-info`). That
is a client change with a test and a line in install.md; it needs no
protocol change.

## 4. Windows

### 4.1 What SmartScreen does without a signature

A file downloaded in a browser carries the Mark of the Web (a
`Zone.Identifier` stream). Opening an unsigned `.exe` or `.msi` that has
one shows SmartScreen's "Windows protected your PC", with the Run
anyway button hidden behind More info. Today's `aries.exe` from the zip
draws the same warning only when run from Explorer; run from a
terminal, the usual way for a CLI, it does not. So, as on macOS, **an
unsigned installer adds a warning to the path it is meant to ease.**

A signature does not end the warning at once. SmartScreen builds
reputation per publisher and per file over downloads; a new
certificate, and a new release's new hash, may still warn for a while.
Extended validation (EV) certificates used to carry reputation from
the first download; Microsoft is reported to have ended that in 2024
(vendor and forum reports, Read; no Microsoft policy page found, so
**Not confirmed**). Whatever the class, it helps to sign `aries.exe`
inside the zip too, so that Defender and SmartScreen see one publisher
on every file.

### 4.2 What Authenticode signing needs

- **A code-signing certificate for the legal entity.** Since 2023 the
  CA/Browser Forum requires its key to be on hardware (a token or an
  HSM), so a `.pfx` in a secret is no longer an option. Two ways fit
  a workflow:
  - **Azure Artifact Signing** (formerly Trusted Signing): Microsoft
    holds the key and issues short-lived certificates after validating
    the organization; about 9.99 USD a month for the Basic tier
    (Microsoft Learn, Read; the product page says free for now, so the
    price is **Not confirmed**). Its eligibility rules for organizations
    (country, years of history) are to be checked on sign-up. It signs
    through `signtool` with its plugin, or Microsoft's Action
    (`azure/trusted-signing-action`), with an Entra application's
    credentials, which can be a federated credential bound to the
    `release` environment, so no long-lived secret is stored at all.
  - **An OV certificate from a CA with a cloud key** (DigiCert KeyLocker,
    SSL.com eSigner and others): a few hundred USD a year, signed
    through the CA's tool or `jsign`.
- **Where the tool runs**: `signtool` is the Windows SDK's and needs a
  Windows runner; `jsign` (Apache-2.0, Java) and `osslsigncode`
  (GPL-3.0) sign from Linux. Either is a tool to admit (§5.2).
- **Where the credential lives**: in the `release` environment, the
  maintainer's (rule 3). The same certificate can sign the desktop
  extension's MCP bundle (identity.md §9), which needs a code-signing
  certificate anyway; one purchase serves both.

### 4.3 Which installer

| | WiX (`.msi`) | Inno Setup (`.exe`) | NSIS (`.exe`) |
|---|---|---|---|
| Output | Windows Installer package | an installer program | an installer program |
| Builds on | Windows (`wix` as a .NET tool) | Windows | Linux or Windows (`makensis`) |
| Adds to `PATH` | Declaratively (`Environment` element), removed on uninstall | By a script in the installer | By a plug-in (EnVar) |
| Per user, no UAC | Yes (per-user package) | Yes | Yes |
| Device management (Intune, Group Policy, SCCM) | Native: silent install, upgrade codes, repair | Silent flags; wrapped as a Win32 app | Silent flags; wrapped as a Win32 app |
| License | MS-RL source; since v6 an Open Source Maintenance Fee is asked of organizations with over 10,000 USD of revenue that use its binaries (WiX's docs, Read) | Inno Setup's own permissive license | zlib |

**WiX's `.msi` fits the product's goal best**: an organization deploys
an `.msi` through its device management without wrapping it, and
Windows Installer removes the `PATH` entry it added. It is the costliest
to build (a Windows runner, the .NET SDK, and the maintenance fee if
Ariesworx LLC's revenue passes WiX's line; §8, 6). **NSIS is the
cheapest**: it builds on the Ubuntu runner the release already uses,
under zlib, but it is an `.exe`, and its `PATH` handling is a plug-in.
Inno Setup sits between and has no advantage here that WiX or NSIS
lacks.

A per-user install (`%LOCALAPPDATA%\Programs\Aries`, the user's
`PATH`) needs no administrator, and `aries upgrade` can still swap the
binary there (`swap.go` already renames the running `.exe` aside on
Windows). Windows Installer would then hold a stale version and may
"repair" the old file back; so, as on macOS, `aries upgrade` should
refuse on an install it did not make and name the new `.msi` (§3.4).

## 5. How it fits the release

### 5.1 The workflow

Today's order is: the `build` job (no secrets) builds the archives,
writes `checksums.txt` and attests their provenance; the maintainer
then starts `sign-and-publish`, which alone reads `ARIES_RELEASE_KEY`,
signs `checksums.txt` and publishes. Apple's and Microsoft's signatures
change the binaries' bytes, so they must come before `checksums.txt` is
written, and they need secrets, which the `build` job must not have.
So platform signing moves the archives' last step into the run the
maintainer starts:

1. `build` (unchanged in trust): builds every archive, unsigned, plus
   the icons and the unsigned `.pkg` and `.msi` inputs, and uploads them.
2. `sign-and-publish`, in the `release` environment, after its checks of
   who started it: a macOS job signs `aries` for darwin with the
   Developer ID, builds and signs the `.pkg`, notarizes and staples it;
   a Windows job (or an Ubuntu job, with `jsign` or NSIS) signs
   `aries.exe`, builds and signs the installer; then the darwin and
   windows archives are repacked with the signed binaries, and only then
   is `checksums.txt` written, attested and signed with the release key,
   as today.

The release key's step, `internal/release/keys.go` and
`internal/tools/releasekey` do not change (rule 3), and
`checksums.txt.sig` still covers every file, the installers included,
so `aries upgrade` and `install.sh` verify them as they verify the
archives. The provenance attestation moves to step 2, since it must
name the files that ship; step 2 then needs `id-token: write` and
`attestations: write`. The independent rebuild (supply-chain.md, SC4)
compares the binaries with their signatures removed
(`codesign --remove-signature`, `osslsigncode remove-signature`), or
the unsigned archives that step 1 keeps, which the release can publish
beside the signed ones.

`install.sh` and `aries upgrade` stay the main path, unchanged, and the
README's install section keeps them first; the installers are a
download for people who want one, listed in install.md under their
own heading.

### 5.2 What it would take in (rule 4)

An agent proposes these and adds none of them, nor their `deps.allow`
lines; each is pinned (an Action by commit, a tool by a version and a
checksum or a `go.sum`) and taken no sooner than 14 days after its
release (30 for a new major version):

| Need | Option | Kind | Scope |
|---|---|---|---|
| Pack `.ico` and `.icns` | `internal/tools/icons`, ours, standard library | none | |
| Embed the icon in `aries.exe` | write the `.syso` resource ourselves (the format is small), or `github.com/tc-hib/winres` | a module, if taken | tools |
| macOS signing and notarization | Xcode on a `macos-15` runner; or `rcodesign` from Linux | a runner image; or a tool | actions / tools |
| `.pkg` | `pkgbuild`, `productbuild` (Xcode) | runner | |
| `.dmg`, if wanted | `hdiutil` (macOS) | runner | |
| Windows signing | `azure/trusted-signing-action`; or `signtool`; or `jsign` | an Action; or a tool | actions / tools |
| Windows installer | WiX (`dotnet tool install wix --version …`); or NSIS (`makensis` from Ubuntu's archive) | a tool | tools |

`internal/depcheck` today holds Go modules, the tools module and every
workflow's `uses:`. A NuGet tool (WiX) or a Java one (`jsign`) is
outside what it reads, and `deps.allow`'s header says programs that
are not Go modules are not listed there. The slice that adds one
extends the file's rule and the test to it, so that nothing in the
release's path goes unlisted; that is part of its cost.

### 5.3 What stays out

- **`ariesd`** gets no installer: it runs on Linux, where
  `install-server.sh` and the archives serve.
- **Homebrew, winget and Scoop.** Not asked, and listed because they
  are the cheapest way to "install like an app": a Homebrew formula in
  a tap of the project's, a winget manifest pointing at the zip (winget
  installs a portable zip onto `PATH`), a Scoop manifest. None needs an
  installer, an icon or, for the formula and Scoop, a platform
  signature (Homebrew fetches with `curl`; winget's zip still meets
  SmartScreen only when run from Explorer). They are worth a design of
  their own if the maintainer wants them (§8, 7).

## 6. Options

| | Option | What people get | Cost | Maintainer's work |
|---|---|---|---|---|
| A | **Icon only** | The mark on the README, the repository and, later, the desktop extension | A designer's fee, or the maintainer's time | Choose the mark |
| B | **A, and package managers** (§5.3) | `brew install`, `winget install`; no warnings | None in fees | Own the tap; approve winget's pull requests |
| C | **A, a signed `.pkg`, and signed darwin binaries** | Double-click install on macOS, device management, no Gatekeeper warning on any download | 99 USD a year | Enroll, make the certificates and the API key, store them, add the signing steps |
| D | **C, and a signed Windows `.msi`** | The same on Windows, through Intune or by hand | C, and about 120 USD a year (Artifact Signing) or a few hundred (an OV certificate), and WiX's fee if it applies | C, and validate with Microsoft or a CA |
| E | Unsigned `.pkg` and installer | A warning on every download | None | Little |

**Recommended: A now, then C, then D, with B whenever wanted; never E.**

- **A first**, because every other option shows the icon, and the
  desktop extension (identity.md, N7) needs one before v1.0.0 whatever
  is decided here. The `internal/tools/icons` slice can land with a
  placeholder, behind the maintainer's choice of mark.
- **C before D**, because the people Aries serves first (developers and
  their agents) are mostly on macOS and Linux, the macOS fee is fixed
  and small, and signed darwin binaries fix the browser-download case
  for the archives too, not only for the `.pkg`.
- **D when an organization asks to deploy on Windows**, since its main
  value is device management, and the certificate serves the desktop
  extension's bundle as well (identity.md §9), which may bring it
  forward.
- **E never** (§3.1, §4.1).

### 6.1 Slices, if chosen

| Slice | Holds | Size |
|---|---|---|
| K1 | The SVG and its PNGs (a person's), `internal/tools/icons` with its test, the icon in the README | 1 day, after the mark |
| K2 | `aries upgrade` refuses an install it did not make, with a fix (§3.4, §4.3) | 1 day |
| K3 | The macOS job: universal binary, Developer ID signature, `.pkg`, notarization, stapling; the darwin archives repacked; install.md | 2 days, and the maintainer's enrollment |
| K4 | The Windows job: Authenticode on `aries.exe`, the `.msi` (or NSIS `.exe`), its icon; install.md | 2 days, and the maintainer's certificate |
| K5 | The `.dmg`, if wanted | half a day |

K2 lands before K3 or K4 ships. K3 and K4 change `release.yml`, and are
the maintainer's to review and merge (rule 3; the memory `merges`).

## 7. Costs

| Item | Cost | Recurs |
|---|---|---|
| Apple Developer Program, organization | 99 USD (Apple, as known in 2026) | yearly |
| D-U-N-S number | free from Dun & Bradstreet for Apple's enrollment | once |
| Azure Artifact Signing, Basic | about 9.99 USD a month (Microsoft Learn, Read; **Not confirmed**) | monthly |
| or an OV code-signing certificate with a cloud key | a few hundred USD (CAs' price lists vary; not read for this) | yearly |
| WiX's Open Source Maintenance Fee | a GitHub sponsorship, if Ariesworx LLC's revenue passes WiX's line | while used |
| A designer for the mark | the maintainer's to set | once |
| A macOS runner | billed by the minute on a private repository; free for a public one | per release |

## 8. Questions for the maintainer

1. **The mark.** Who draws it: the maintainer, a commissioned designer
   (rights assigned to the copyright holder), or an agent's placeholder
   until one is chosen?
2. **Its license.** The icon under AGPL-3.0-only with the code, or kept
   apart as a mark, with a line in the README saying forks use their
   own?
3. **The publisher's name.** An Apple Developer ID shows "Developer ID
   Application: *the legal entity* (TEAM)", an Authenticode certificate
   names the entity, and an `.msi`'s `Manufacturer` and a `.pkg`'s
   distribution file name a publisher in files in this repository.
   Rule 1 names Ariesworx LLC only in the README's license notice
   (decision 82). Widen that exception to the installers' publisher
   fields, or give them a neutral "Aries" and leave the entity to the
   certificates, which are not in the repository?
4. **macOS.** Enroll in the Apple Developer Program (option C)? A
   `.pkg` alone, or also a `.dmg` around it?
5. **Windows.** Option D now, later, or only when an organization asks?
   Azure Artifact Signing or a CA's OV certificate?
6. **Which Windows installer.** WiX's `.msi`, with its Windows runner
   and its maintenance fee if it applies, or NSIS's `.exe` from Ubuntu?
7. **Package managers.** A Homebrew tap and a winget manifest, as a
   separate design (option B)?
8. **Where platform signing runs.** In the `sign-and-publish` run, with
   the attestation moved there (§5.1), or a third run the maintainer
   starts between build and sign?
