# Reaching an Aries server: research on sign-in, identity and operations

13 Oct 2026. **Status: research. The maintainer decided its fourteen questions on 14 Oct 2026 (§11; [aries.md](aries.md) §14, decisions 52 to 65), each as recommended; of what it proposes, only the server installer is built ([§8](#8-deploying-and-maintaining-it), As built, 14 Oct 2026).** No product code, setting or guide was changed for it. It follows the identity design of [aries.md §17](aries.md#17-identity-certificates-handles-and-provisioning), which is also unbuilt, and takes up what §17.10 left out: signing in to Aries itself.

The question, in the maintainer's words: "It is getting harder and harder to maintain these SSH keys across multiple machines. I am thinking we may need to give the aries client a SSO link in the terminal and store it to make management easier", for several providers (Google, Microsoft 365, GitHub), and then how access could be made secure and easy for a development and operations team to deploy and maintain.

Every statement below is one of three kinds, and says which where it matters:

- **Measured**: run on 13 Oct 2026 in throwaway Docker containers ([§3](#3-what-was-measured)).
- **Read**: from a provider's or project's own documentation on 13 Oct 2026, cited in [§13](#13-sources). The pages were read through a tool that summarizes, so wording is paraphrased and no sentence here should be taken as a quotation.
- **Inferred**: reasoning from the first two, or general knowledge, not checked.

Nothing was run against a real provider. No account was created and nobody signed in anywhere ([§12](#12-not-verified)).

## 1. Summary

### What was found

1. **Aries does not need a certificate authority, or a signing key, to give short-lived access after a sign-on.** sshd honors `expiry-time="…"` on a line of the file ariesd already writes (measured). A sign-in can therefore end in an ordinary key line that expires: the control store records a fresh public key for the principal with an end time, the file carries it, and everything decision 22 built (the fingerprint on the connection, `key remove` closing connections, the daemon's check at the handshake) applies unchanged. This is smaller than the flow first proposed to the maintainer, which had Aries sign SSH certificates with an authority key of its own.
2. **The pre-authentication problem has a small answer.** The flow proposed needs a way for a machine nobody knows to hand a token to the server. Three ways were measured and all work: a key command that admits any key to an enrollment command only, an account that needs no authentication, and (the same thing with no sshd change) a published key whose line forces an enrollment command. None needs a new port.
3. **The device authorization grant is the wrong default.** It works with Google, Microsoft Entra ID, Okta, GitLab and GitHub (read), but: its token cannot be bound to the machine's key (measured against Dex: a `nonce` sent with the request did not come back in the ID token); it is the flow device-code phishing abuses; and Microsoft recommends blocking it, ships a managed Conditional Access policy that does, CISA's baseline says it should be blocked, and the FBI issued an alert on it in May 2026 (read). In the enterprises Aries is aimed at it will often be switched off. The flow to build first is the one `gcloud`, `az` and `opkssh` use on a machine with a browser: authorization code with PKCE to a loopback address, with a nonce that commits to the new key. The device grant stays as an option a server admin turns on, for machines with no browser.
4. **OpenPubkey (`opkssh`) does what it says, and does not fit as it stands.** Measured end to end against a mock issuer: the ID token rides inside a certificate the client signs itself, sshd hands it to an `AuthorizedKeysCommand`, and there is no authority and no pre-authentication path. But the line it prints has no forced command (a shell, unless wrapped), it needs a browser on the same machine, it has no sign-in for GitHub users, every sign-in at sshd fetches the issuer's keys, and it brings back the `AuthorizedKeysCommand` that decision 22 removed for good reasons. Its idea, the nonce that binds a token to a key, is worth taking; the tool is worth using as a stopgap, not as the design.
5. **Identity must be keyed on the provider's permanent identifier, never on email.** Issuer and `sub` for Google and generic OpenID Connect; tenant and `oid` for Entra (its `sub` differs for each app registration); the numeric id for GitHub. Email is what an admin types in an invitation and what is shown. The "nOAuth" class of takeovers, and the reuse of a lapsed company domain, are what email as a key costs (read).
6. **Brokers in front of sshd solve the network, not the identity.** Google IAP, AWS Session Manager and EC2 Instance Connect Endpoint, Azure Bastion, Tailscale as a network and WireGuard all leave the machine's sshd authenticating a key, so the principal still comes from the key line. The ones that replace or front sshd with their own identity (Tailscale SSH, Cloudflare Access for Infrastructure, Teleport, OS Login, Entra login for Linux) map a person to a Unix account and have no place for a forced command per person, so they cannot carry an Aries principal without new code.
7. **The same enrollment serves agents and CI with no browser**: a workload's own token (GitHub Actions, a cloud instance, a Kubernetes service account) is exchanged for a key that lasts the job.

### The options

| | A. Keys, better tooling | B. Certificates from a company's authority (§17) | C. Aries signs people in (`aries login`, expiring keys) | D. `opkssh` verified by sshd | E. A broker in front |
|---|---|---|---|---|---|
| A person on a new laptop | Makes a key (or unlocks one from a synced agent or a hardware key) and gets an admin to add it | Runs the company's tool (`step ssh login` or the like); nothing in Aries | `aries login`: a browser tab, the company's sign-on, done | Installs `opkssh`, runs `opkssh login`; `aries` then uses the key and certificate | Signs in to the broker's client; still needs a key or one of B, C, D for who they are |
| An admin adds someone | `principal add NAME --key FILE`, and `key add` for each further machine | `principal add --external alice@example.com`, once | `principal add --external alice@example.com`, once (an invitation) | A line of a policy file on the server, as root | The broker's group or role, and the Aries step of the column used with it |
| An admin removes someone | `principal disable`: at once | `principal disable`: at once. Doing nothing: within a certificate's lifetime | `principal disable`: at once. Doing nothing: within a sign-in's lifetime | Delete the policy line; the cached certificate works until the policy's lifetime ends | The broker cuts the network path at once; Aries's own step still needed |
| An operator runs | Nothing new | The authority (step-ca, Vault, Teleport, a cloud's), three more sshd lines, its public key in the config | One app registration at the provider, a `login:` block in the config, outbound HTTPS from the server | `opkssh` on the server as sshd's key command with a wrapper, a system user, two policy files, outbound HTTPS | The broker's agent on the server and on every client, CI included |
| An agent or CI job | Its own key in a secret store, as today | A certificate from the authority for its workload identity | Its workload token exchanged for a key that lasts the job | GitHub Actions and GitLab CI tokens are supported by `opkssh`; others not | A machine identity at the broker, plus its Aries identity |
| What must be trusted | Each machine's disk; each admin's judgment for each key | The authority and everyone who can configure it | The provider's tenant; ariesd's verifier; a pre-authentication path | The provider's tenant; `opkssh` (0.17, no audit found) running before authentication | The broker's vendor and control plane |
| Cost to build | Days (enrollment by code), or none (a synced agent) | A stage: §17 slices I2 to I4 | A stage: §17's I2, then about as much again | Days for a wrapper and a guide; then a dependency to track | None to a few days (a transport, as IAP has) |
| Small team, no platform group | Works until the fourth machine; the pain reported | No: nobody to run an authority | **Yes**, with GitHub or Google as the provider | Possible today as a stopgap | Tailscale or WireGuard is a good network layer; not an identity |
| Enterprise with a platform group | No | **Yes**, where an authority exists | **Yes**, where none does | Unlikely to pass review yet | Usually required as well, for the network |

### Recommendation

**Build C in the smaller form found here, keep B as designed for companies that already issue SSH certificates, and treat A, D and E as things to document, not build.** In one sentence: `aries login` signs a person in at their provider and the server answers by recording a fresh public key for their principal that expires, written to the file sshd already reads.

The reasons:

- It removes the admin from every new machine, which is the pain reported, and it does so for a team with no platform group, which B cannot.
- It adds no mechanism to sshd. Keys stay lines of one file; expiry is a line that is no longer written. What is in §15.2 keeps working as measured there.
- It keeps the rule that makes Aries's model sound: the principal comes from the line that authenticated, the control store decides, and nobody adds a key to their own account by anything an agent holding their key could do alone. The provider's sign-on, with its second factor, is the approval decision 19 asks another admin for.
- It holds no long-lived secret anywhere: no authority key on the server, no client secret that is a secret, and a private key on each machine that is worth hours.
- B and C share their first half (§17's handle, display name and external identity, and the client fixes), so nothing built for one is thrown away by the other.

What it costs, plainly: ariesd gains a verifier of identity tokens and, for the first time, a reason to make outbound requests to the internet (the provider's signing keys); a path that strangers can reach before they are known; one adapter for each kind of provider, which providers will keep changing under it; and a class of attack (a phished sign-in) that keys do not have. An enterprise that already has an authority should use B and never turn C on.

**Order of work.** Slices named I*n* are §17.11's.

| Step | What | Why here |
|---|---|---|
| 0 | No code: tell teams that one key in a synced agent or on a hardware key is one `key add` for all of a person's own machines ([§4.1](#41-keys-as-today-with-better-tooling)) | Relieves the pain reported, for laptops, this week |
| 1 | I1, the client (`key:` exclusive, `whoami`, the two refusals reworded), which is already decided | Common to every option |
| 2 | I2: the handle, display name and external identity, with the identity widened from one string to (kind, issuer, subject) and an invited address ([§6](#6-managing-users)) | Common to B and C; useful with keys alone |
| 3 | **Spikes, before any design is accepted** ([below](#what-to-spike-first)) | The design turns on three things only a real tenant and a real server show |
| 4 | Expiring keys: a key row with an end time, `expiry-time` on its line, the daemon's check on the store's clock, pruning | The smallest slice; useful alone, for a contractor's or a CI key that should lapse |
| 5 | Enrollment: the pre-authentication path, the token check, `aries login` with the loopback flow, generic OpenID Connect (covers Google, Entra, Okta, Keycloak, GitLab) | The feature |
| 6 | Workload tokens for agents and CI ([§7](#7-agents-ci-and-unattended-sessions)), with decision 8's agent identities | Removes the last long-lived key, the CI one |
| 7 | The GitHub adapter (no ID token: the server calls GitHub's API) and the device grant as a server option | Small teams; headless machines |
| 8 | I6, provisioning from a directory by a job | Leavers, without waiting for a lifetime |
| later | I3 and I4 (certificates), when a company with an authority asks | Designed; waits for demand |

### What to spike first

1. **A real tenant of each provider.** Google Workspace, Entra ID and a GitHub App, each registered by the maintainer: the loopback flow with PKCE and a nonce, which claims arrive, whether Entra's managed policy already blocks the device grant in a fresh tenant, and whether Google's device grant accepts a nonce. An afternoon each. Nothing in §5 was run.
2. **The pre-authentication path on the guide's server**, under systemd and AppArmor: the published-key line against the any-key command, what a stranger can do to the bridge and the daemon through it, what sshd logs, and what limits hold.
3. **`expiry-time` across what the guide supports**: its time zone (the test server's clock was UTC, so the measurement does not show which zone sshd reads the value in), and that a line ariesd stops writing is gone within a tick.
4. **`aries` with an `opkssh` certificate**, and `opkssh` as sshd's key command on the maintainer's own server for a week, with the wrapper of §3. Not as the design: to feel a sign-on in daily use, and to learn where it rubs, before building one.

## 2. How access works today, and the operator's journey

Only SSH (decision 9). sshd authenticates a key for the one account `aries`; the key's line forces `ariesd stdio --principal NAME [--key FINGERPRINT]`; on a server of many projects the keys are rows of the control store, written by ariesd to `.ssh/aries_keys`, which sshd reads before the hand-maintained `authorized_keys` (decisions 22 and 26). `aries` offers `ARIES_KEY_FILE`, or an in-repository `key:`, and then nothing else; with neither, the default identities of `~/.ssh` and then ssh-agent's keys (decision 35). The server's host key is pinned in a committed `.aries.yaml`. A server with no public address can be reached through Google Cloud IAP, which carries the bytes and changes nothing else.

**The journey today**, counted from [Running a server](../server.md#set-up-a-server):

1. Install Dolt, comparing its checksum.
2. Create Dolt's account, config and unit.
3. Create the `aries` account and install ariesd with its signature checked.
4. Create the database and its account, and write the config file with `admins:`.
5. Install ariesd's unit and the sudo rule for restarts.
6. Add the first admin's key line to `authorized_keys`, install the sshd `Match` block, read out the host key's fingerprint.
7. Commit `.aries.yaml` to a repository.
8. Create and grant the control database.
9. Name it in the config.
10. `ariesd control init`.
11. Grant a block of project database names.
12. Let ariesd write the keys file (a unit drop-in) and have sshd read it (one line).
13. Restart.

Thirteen steps, plus the optional ones (AppArmor, backups, telemetry, a firewall rule). Then, forever: one admin command for each person, **and one more for each further machine of each person**, each needing a public key file carried to an admin by some channel.

Where a secret or a trust decision lives:

| What | Kind | Where |
|---|---|---|
| The Dolt archive and the ariesd release | Trust | A checksum compared by hand; a signature the installer checks |
| Dolt's `root` password | Secret | Generated and forgotten at step 4 |
| ariesd's database password | Secret | `/etc/aries/ariesd.yaml`, mode 0600 |
| `admins:` | Trust | The same file; read at start |
| The first admin's key line | Trust | `authorized_keys`, by hand; also the break-glass way in |
| The host's private key | Secret | `/etc/ssh` |
| The host key's fingerprint | Trust | Every repository's committed `.aries.yaml` |
| The sudo rule | Trust | `/etc/sudoers.d/aries` |
| Each person's private key, on each machine | Secret | People × machines of them, none expiring |
| Each public key added | Trust | An admin's decision, once for each machine, on the word of whoever sent the file |
| A CI key | Secret | The CI system's secret store, not expiring |

Two secrets on the server. The rest of the secrets are the keys this document is about, and the last two rows of trust are the ones that grow.

## 3. What was measured

All on 13 Oct 2026, on one Apple silicon Mac under Docker Desktop (engine 29.8.2), in three containers on a network of their own, all removed afterwards with their images, network and volume. The scripts and their output are not in the repository.

| Part | Version |
|---|---|
| sshd and `ssh` | OpenSSH 9.6p1 (Ubuntu 3ubuntu13.19), in `ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55` (24.04) |
| `opkssh` | 0.17.0, the release's `opkssh-linux-arm64`, SHA-256 `2e5006fcb88bae344bd03bd00f749cce32509a8f20e6f66c29ff50f6bdb4e343`, checked against the release's `checksums.txt` |
| The issuer | Dex 2.46.0, `ghcr.io/dexidp/dex@sha256:933fcd3f523338c847b88ef84a7145fb2fcb7b9917f08418612afeaaa2001777`, with two static users, `alice@example.com` and `bob@example.com`, one public client with no secret, and a throwaway TLS authority that both other containers trusted |
| The "browser" | `curl`, following Dex's login and approval forms |

Dex was chosen because it is open source, small, pinned by digest, and is a plain OpenID Connect issuer with both the authorization code flow and the device grant. It stands in for a provider; it is not one. ariesd did not run: a small script stood in for the forced command and recorded its arguments and environment, as in §17.1. The account, the `Match User aries` block and the two key files were the guide's, with `AuthorizedKeysCommand` pointed at a test script run as a dedicated user, and `ExposeAuthInfo yes`.

**1. sshd, a key command and the two files together.**

| Setup | Result |
|---|---|
| The same key in `aries_keys`, in `authorized_keys` and printed by the command, each with a different forced command | The line of `aries_keys` ran. **The command was not executed at all.** sshd consults the files first, in the order listed, and runs the command only for a key no file matches. This agrees with the current manual page, which says the command is tried after the files; an older wording had it the other way round |
| The key in neither file, printed by the command | Admitted, and the command's forced command ran. sshd ran the command twice for the sign-in (once when the key was offered, once when it was used), as §15.2 saw |
| What sshd passes for `%u %k %t %f` | The account's name; the whole key, or for a certificate **the whole certificate** (2,660 base64 characters here); the type, which says whether it is a certificate; the fingerprint of the key |
| A line with `expiry-time="YYYYMMDDHHMMSS"` one minute ahead, in `aries_keys` | Admitted before that time. After it, the line no longer matched and sshd went on to the next source, as if the line were absent |
| A command that prints, for any key at all, `restrict,command="… enroll --key FINGERPRINT" TYPE KEY` | A key made that minute was admitted to that command and no other. The forced command saw the fingerprint in its arguments and the key in `SSH_USER_AUTH`. A key that was in a file still got its own line, and the command was not asked |
| An account with an empty password, `AuthenticationMethods none`, `PermitEmptyPasswords yes`, a `ForceCommand` | With `PasswordAuthentication no` (set globally): refused. With `PasswordAuthentication yes` in the account's `Match` block: admitted with no credential (`Accepted none`), the forced command ran, and `SSH_USER_AUTH` was empty |

**2. `opkssh`, end to end.**

| Tried | Result |
|---|---|
| `opkssh login` with an `http://` issuer | Refused: the issuer must be `https://` |
| `opkssh login` against the TLS issuer, as alice | A new ECDSA key and `id_ecdsa-cert.pub`, 2,712 bytes. The certificate is **signed by its own key**, has key id `alice@example.com`, serial 0, **valid forever**, the one principal `opkssh-wildcard`, the five default extensions, and an extension `openpubkey-pkt` holding the token (1,382 bytes) |
| What the token holds (`opkssh inspect`) | The provider's ID token (`iss`, `sub`, `aud`, `email`, `email_verified`, `iat`, `exp`, and a `nonce`), the provider's signature, and a second signature by the client's key over a statement of that key. The nonce is the commitment to the statement: this is what binds the token to the key |
| `ssh` to the server, `opkssh verify` as the key command | Admitted. sshd asked the command about the bare key first (refused: not a certificate), then the certificate. `opkssh` printed `cert-authority,principals="opkssh-wildcard" KEY`: the client's own key as an authority for that one connection |
| That line as printed | **No `command=` and no `restrict`: the command the client asked for ran.** With the guide's `Match` block there was no terminal and no forwarding, but it is a shell as the account, which §17.1 already warns of |
| A wrapper that inserts `restrict,command="…"` into the line | The forced command ran, and read the whole certificate, token included, from `SSH_USER_AUTH` (mode 0600, the account's) |
| bob signs in at the issuer; the policy file names only alice | Refused by `opkssh verify`: no policy lets `bob@example.com` assume `aries` |
| alice's certificate offered with bob's private key | sshd found the certificate acceptable; the client could not sign for it; refused |
| Time for one `opkssh verify`, issuer on the same Docker network | 12 to 13 ms, three runs. It fetches the issuer's discovery document and keys each time. A whole `ssh` sign-in took about 0.1 to 0.2 s with or without it |
| The issuer stopped | Refused: `failed to fetch JWK set`. A plain key in `authorized_keys` was admitted as before |
| The issuer restarted with new signing keys | Every certificate from before was refused: `no matching public key found for kid`. A provider's key rotation ends such certificates early |
| Policy `oidc`, a two-minute ID token | Admitted while the token was valid; 135 s after sign-in, refused: `the ID token has expired`. sshd's own check plays no part: the certificate never expires |
| The same certificate, policy `24h` | Admitted, with the ID token expired. The lifetime is the server's policy measured from `iat`, on the server's clock |
| Policy `oidc_refreshed`, no refresh token sent | Refused |
| `opkssh login` with one-minute ID tokens at the issuer | Failed at the client: `token has expired`. With two minutes it worked. Not investigated |

**3. The device grant.**

| Tried | Result |
|---|---|
| A device authorization request by the public client with no secret, then sign-in as bob in the "browser" | The token endpoint answered `authorization_pending`, then an access token and an ID token for bob |
| The same request with `nonce=…` added | **No `nonce` in the ID token.** RFC 8628 defines none for this grant; this issuer ignores one. The token names a person and a client, and nothing ties it to the machine that asked |
| A second poll with the same device code after success | The tokens again. Dex does not make a device code single-use; a provider may |

**What the measurements settle.** A key command and the two files coexist in one `Match` block, files first. A key line can expire by itself. A machine nobody knows can be given exactly one command. A self-signed certificate is exposed whole to the forced command. And a token from the device grant is a bearer token.

## 4. The options for sign-in

### 4.1 Keys as today, with better tooling

Nothing changes in the model: a key names a principal, an admin adds it.

- **One key for all of a person's own machines.** An agent that syncs keys (1Password's keeps the private key in its app, asks for approval, and offers the same key on every machine the person signs in to; read) or a hardware key (`ed25519-sk`, OpenSSH 8.2 and later, with `verify-required` for a PIN since 8.4; read) makes a person one key and one `key add`. `aries` already takes its signers from ssh-agent. Secretive keeps a key in a Mac's Secure Enclave, which is safe and is one key for each Mac, so it does not reduce the count. *Limits:* Go's SSH library has no signer for `sk` keys of its own, so such a key works only through an agent (read; that it works through an agent with `aries` is inferred and untested, as §17.13 already says); an agent that asks for approval on every new process is tiresome with a hundred sessions; and none of this reaches a headless machine or a cloud sandbox.
- **Agent forwarding** reaches the headless machine with no key on it, and is the wrong answer: anyone with root on that machine can use the forwarded agent while the session lasts (read, `ssh_config`). OpenSSH 8.9's destination constraints narrow that (read), but they depend on a session-binding extension that Go's agent client does not mention (read; inferred that a constrained key would then fail from `aries`). Not recommended.
- **`aries` enrolls a key itself, with an approval.** The new machine makes a key and reaches the server by the pre-authentication path of §4.3; the server holds the key as pending and shows a short code; an admin runs `aries admin key approve CODE`. This saves carrying a `.pub` file and nothing else: decisions 19 and 38 mean the approver cannot be the person's own other machine, since an agent holding their key could approve a key of its own. Days to build once the path exists; a modest gain.

*For a person:* fine on laptops with a synced agent. *For an admin:* still one step for each key. *For an operator:* nothing. *For an agent or CI:* a key in a secret store that never expires. *Failure modes:* a key on a lost laptop works until someone remembers; nothing ties Aries to a second factor. *Cost:* none to days.

### 4.2 Certificates from a company's own authority

§17 as designed, and this research changes none of it. What was added by reading:

| Authority | People | Workloads | Where the certificate lands | License |
|---|---|---|---|---|
| step-ca | An OpenID Connect provisioner; `step ssh login` | Its cloud provisioners sign host certificates only; user certificates for workloads need another provisioner and a template (inferred) | In ssh-agent, which unmodified `aries` uses (§17.1) | Apache-2.0 |
| Vault's SSH engine, and OpenBao's | Whoever Vault authenticates (OIDC, JWT) | The same, by Vault's JWT login (not confirmed for GitHub Actions) | A file beside the key, which needs §17.9's client change | Vault: BSL 1.1 since 2023. OpenBao: open source |
| Teleport | Its own sign-on; in the free edition GitHub only | `tbot`, with many join methods, one-hour credentials renewed every twenty minutes | Its own client and agent | Community binaries are free only under 100 employees and under 10 million US dollars of revenue; OIDC and SAML are in the paid edition |
| Google OS Login certificates, Entra's `az ssh` | The cloud's identity | Service accounts | Files | The cloud's; and each maps a person to a Unix account of their own, not to `aries` |

*For a person:* the company's tool once a day; a new laptop is nothing. *For an admin:* one external identity for each person. *For an operator:* an authority to run, or one that exists. *For an agent:* a certificate for its workload identity. *Trust:* everyone who can configure the authority. *Cost:* §17's I2 to I4. *Fit:* right where an authority exists; a team with no platform group will not stand one up to try Aries, which §17.12 already notes.

### 4.3 Aries signs people in

**The flow first proposed**, and what the research changes in it:

| Step proposed | Finding |
|---|---|
| `aries login` prints a link and a code (the device grant) | Works with every provider looked at. But it should be the fallback: see finding 3 of the summary and [§9](#9-threats). Default to authorization code with PKCE and a loopback redirect, which needs a browser on the machine and takes a nonce |
| `aries` gets an identity token | Yes for Google, Entra, Okta, GitLab, Keycloak. **Not for GitHub**, which returns only an access token |
| `aries` makes a fresh key pair and hands token and public key to the server | Yes. With the loopback flow the nonce can be a hash of the new public key and a challenge from the server, so the token is good for that key and that server only. With the device grant it cannot |
| The server checks the token and finds the principal | By (kind, issuer, subject) in the control store, never by email ([§6](#6-managing-users)) |
| The server signs the key into a short-lived certificate with an authority key of its own | **Not needed.** The server records the key with an end time and writes a line with `expiry-time` (measured). No signing key exists to protect, rotate or leak |
| `aries` stores it | A private key in the person's own state directory, mode 0600, named by the server; used as a configured key is (decision 35). Nothing is committed |
| The hard part: nothing reaches the server before sshd authenticates | True, and smaller than feared: below |

**The pre-authentication path.** Four candidates.

| Path | sshd change | What a stranger reaches | Proof that the caller holds the new key | Verdict |
|---|---|---|---|---|
| **A published enrollment key.** ariesd writes one more line to its keys file: a well-known public key, forced to `ariesd stdio --enroll`. `aries` holds the private half, which is not a secret | None | The bridge and the daemon's enrollment handler, as the `aries` account, with no terminal and no forwarding | None from sshd: the enrollment exchange must carry the new public key and a signature by it over the server's challenge | **Recommended to spike.** No new sshd mechanism; nothing of Aries runs before sshd authenticates, as decision 22 has it. Its cost is appearance: a scanner or an auditor will ask about a known private key, as they do about Vagrant's. Deriving the pair from the server's host key fingerprint, which `aries` already pins, keeps it out of any file |
| **A key command that admits any key** to the enrollment command (measured) | `AuthorizedKeysCommand` and its user return to the `Match` block. The command can be a root-owned script of a few lines that only formats its arguments, so it needs no socket to the daemon and no copy of the binary | The same | From sshd itself: the key in `SSH_USER_AUTH` is one the caller just signed with | Works, and proves possession for free. But it undoes `AuthorizedKeysCommand none`, the line that keeps a cloud image's own key command away from the account, and sshd logs `Accepted publickey for aries` for every stranger, which will confuse whoever reads that log |
| **An account with no authentication** (measured) | A second account, with `PermitEmptyPasswords yes` and `PasswordAuthentication yes` in its block | The same, as another account | None | No. The same reach as the first row, for two settings every hardening baseline forbids |
| **A first-use key approved out of band** | The any-key command, or the published key | The same | As above | This is §4.1's enrollment by code. It is the fallback when no provider is configured, not a sign-on |
| **An HTTPS endpoint on ariesd** | None | A new listener, with a certificate to obtain and renew | In the exchange | Deferred by decision 9, and rightly for this purpose: it adds a port, a certificate and a second transport to solve what a line of a file solves. It would be needed for SCIM pushed by a directory ([§6](#6-managing-users)) |

Whatever the path, the enrollment handler is reached by people nobody knows. It must do nothing before the token verifies but answer with the server's provider settings and a challenge; take no connection slot and open no store, like the keys lookup of §15.2; and be capped and rate-limited by address and in total (rule 18), since sshd's `MaxStartups` and its penalties no longer bound a caller who authenticates successfully. The bridge's AppArmor profile already allows only the config file and the daemon's socket, and needs no change.

**End to end.**

- *A person on a new laptop:* `aries login` in a repository. `aries` connects by the enrollment path, learns the provider's issuer and client id from the server (so nothing about the provider is in `.aries.yaml`, and the pinned host key vouches for the answer), opens the browser, and a minute later holds a key that works until the sign-in's lifetime ends. Then `aries login` again, or silently if the server allows a refresh token.
- *An admin adds someone:* `aries admin principal add alice --external alice@example.com --provider google`, once, and never again for that person's machines. *Removes:* `principal disable`, at once.
- *An operator:* registers one application at the provider (below), writes a `login:` block naming the issuer, the client id and the tenant or domain allowed, and lets the server reach the issuer on port 443.
- *An agent at a desk:* uses the person's signed-in key, as it uses their key today. *An unattended agent or CI job:* [§7](#7-agents-ci-and-unattended-sessions).

**Security properties.** No long-lived private key on any machine; a second factor if the provider enforces one; a leaver's access ends within a lifetime with no action, and at once with `principal disable`; every sign-in is a control-store event with the provider, the subject's handle and the key's fingerprint. **Failure modes.** The provider is down: nobody new signs in, sign-ins already made work until they expire, and the break-glass key in `authorized_keys` is untouched. The server cannot reach the provider's keys: the same. A phished sign-in ([§9](#9-threats)). **Cost.** A stage. The verifier is small (RS256 and ES256 over a fetched key set, a dozen claim checks) and could be written on the standard library; a maintained library is the safer choice and is a dependency to justify under rule 4. Each provider is settings and a page of the guide, except GitHub, which is code.

### 4.4 Identity tokens verified by sshd itself: OpenPubkey and `opkssh`

**What it is.** OpenPubkey is a protocol, a Linux Foundation project since 2023, with a paper (IACR ePrint 2023/296). `opkssh` is its SSH tool, Apache-2.0, opened by Cloudflare in March 2025 after it acquired the company that wrote it (search summaries, not read at the source); 0.17.0 was released on 6 Oct 2026, the repository had about 2,000 stars and 64 open issues, and it is still numbered 0.x. No independent security audit of `opkssh` was found.

**How it works** (measured, §3). The client makes a key, signs in with the authorization code flow, and sends as the nonce a commitment to its key. The provider's ID token, the client's own signature and the key are packed into an SSH certificate that the client signs with that same key. sshd passes the certificate to `opkssh verify %u %k %t`, which checks the provider's signature against keys it fetches, the nonce against the key, the audience against a file of allowed issuers and client ids, and the identity against a policy file; it prints the client's key as a `cert-authority` for the connection. No authority, no enrollment, no signing key, nothing stored on the server for each person but a policy line.

**Providers** (read): Google, Microsoft, GitLab and a list of self-hosted issuers (Keycloak, Authentik, Authelia, Zitadel, Kanidm and others), any issuer by `--provider=ISSUER,CLIENT_ID`; GitHub Actions and GitLab CI tokens for workloads. **GitHub users: no**, since GitHub issues people no ID token.

**Weaknesses**, measured and read:

- The line it prints carries no forced command. For Aries it must be wrapped, and the wrapper becomes the thing that decides the principal.
- It needs a browser on the machine that signs in. There is no device grant, and could not be one that keeps the binding.
- The certificate never expires at sshd. The lifetime is the server's policy over the token's `iat` (24 hours by default, which outlives the provider's own one-hour token), on the server's clock.
- Every sign-in at sshd runs a program, before authentication, that makes outbound requests. Issuer unreachable: refused. Issuer's keys rotated: every earlier certificate dead.
- The ID token is a bearer token for the client id it names, and the server sees it. Its documentation says not to share a client id with any other service for that reason, and that its built-in client ids are not for production.
- A certificate of 2.7 KB is offered on every connection, and it carries an email address into sshd's log.
- A third party's analysis (arXiv 2307.16607, from a search summary) notes that whoever runs the client can ask for a token bound to a key of their own; in Aries's terms, an agent holding a person's browser session could.

**Could ariesd be the verifier?** Yes, in two ways, neither attractive:

1. *`AuthorizedKeysCommand` runs an ariesd subcommand that checks the token and prints the line with the forced command.* This is `ariesd keys` of stage 3a over again, which decision 22 removed after running it: sshd requires the program and every directory above it to be root's, while `aries` owns the binary so that upgrades need no root; it ran before authentication for every stranger; and here it would also need the network, or a socket to a daemon that has it.
2. *A static key command admits any self-signed certificate to `ariesd stdio`, and the daemon verifies the token it reads from `SSH_USER_AUTH`.* Measured to be possible: the forced command sees the whole certificate. The daemon then decides, as it does for everything else. But the key command is still required (the `cert-authority` key differs for every client, so no file can hold the line), the token travels on every connection, and GitHub and headless machines are still left out.

Against C, it saves the enrollment round trip and one table of expiring keys, and costs a key command, a browser on every machine and a provider with nonces. **What to take from it** is the nonce that commits to the key, which C's loopback flow can use as it is. **What to use it for** now is a trial: `opkssh` with a wrapper is a way to live with a sign-on on one's own server this month.

### 4.5 Access brokered outside Aries

What the machine's sshd, and so ariesd, sees behind each (read, except where marked):

| Broker | Who it authenticates | What reaches sshd | Can the principal still come from the connection? | `aries`'s own SSH client | Cost |
|---|---|---|---|---|---|
| Google Cloud IAP TCP forwarding | A Google identity with an IAM role | A TCP stream from IAP's range. No identity | Yes, from the key, as today. Supported | Yes, through `gcloud` on standard input, as built | No charge (inferred) |
| Google OS Login | A Google identity | Its own key command and a Unix account for each person | No: it replaces the account, and the guide's `AuthorizedKeysCommand none` keeps it out of `aries` | n/a | The cloud's |
| AWS Session Manager, SSH document | An IAM principal | A stream; sshd still authenticates a key. The IAM identity does not reach sshd (inferred) | Yes, from the key | Would need a helper as IAP has (`aws ssm start-session`) | No charge on EC2 |
| EC2 Instance Connect | An IAM principal | A public key valid for 60 seconds, through its own key command | Not for `aries` with the guide's block | n/a | No charge |
| EC2 Instance Connect Endpoint | An IAM principal | A stream; one hour at most for a connection | Yes, from the key | A helper (`open-tunnel`) | No charge |
| Azure Bastion, native client tunnel | An Entra identity | A stream | Yes, from the key | A helper (`az network bastion tunnel`); Standard tier or higher | Hourly |
| Entra login for Linux (`az ssh`) | An Entra identity | A short-lived certificate from Microsoft, for a Unix account of the person | No | n/a | The cloud's |
| Tailscale as a network | A node, owned by a person or a tag | A stream from a tailnet address. `tailscale whois` on the server can name the node's owner | Yes, from the key. From the node's owner only with new code that asks the local daemon | Yes if the machine runs Tailscale; or a helper (`tailscale nc`) | Free to 6 users, then per user each month |
| Tailscale SSH | A node | **Nothing: `tailscaled` answers port 22 itself.** No `authorized_keys`, and its documentation says it does not suit forced commands | No | No | The same |
| Cloudflare Access for Infrastructure | An identity at Cloudflare's sign-on | A certificate from Cloudflare's authority, for a Unix account; the session passes through Cloudflare | Only as certificates under §17, with the principal Cloudflare chooses (a Unix name or an email's prefix) | Only with Cloudflare's client on the machine | Per user above a free tier (inferred) |
| Teleport | Its own sign-on | Its proxy ends the client's session and opens its own to sshd with its certificate | Only under §17, as its logins | Through `tsh`'s proxy command | See §4.2 |
| WireGuard | A device key | A stream from a private address | Yes, from the key | Yes | None |

So there are two kinds. *Tunnels* (IAP, Session Manager, the Endpoint, Bastion, Tailscale as a network, WireGuard) carry bytes and are compatible with everything else in this document: they decide who reaches port 22, and an enterprise will want one whatever Aries does. Each new one that needs a helper process gets what IAP got: validated names, never a command line in a committed file (§2 of the design). *Replacements* put their own idea of a user where the forced command was. Of those, the ones that issue certificates are §17 with a particular authority; the others are not usable.

One more route was considered and set aside: reading the peer's identity from Tailscale on the server and taking the principal from it. It would make the tailnet's owner the identity provider with no sign-in at all, which is pleasant, and it ties Aries's identity to one vendor's daemon and to an address, where every other route ties it to a key.

## 5. Each provider

All read, none run. "Stable identifier" is what Aries would store.

| | Google Workspace | Microsoft Entra ID | GitHub | Generic OpenID Connect (Okta, Keycloak, GitLab) |
|---|---|---|---|---|
| Device grant | Yes, for a "TVs and Limited Input devices" client. Scopes limited to `openid`, `email`, `profile` and a few of Drive and YouTube. Codes last 30 minutes | Yes, with "Allow public client flows" on. Codes last 15 minutes. No `verification_uri_complete`. **Microsoft recommends blocking it, and a Microsoft-managed Conditional Access policy does** | Yes, turned on for each app. Codes last 15 minutes; 50 code entries an hour for each app | Okta: yes, for a native app with the grant ticked, 10 minutes. GitLab: generally available since 17.9. Keycloak: a switch on the client (from memory) |
| Does a public client need a secret? | **Yes, the request must carry `client_secret`.** Google's own text says of installed applications that the secret is plainly not treated as one; it does not name this client type, and RFC 8628 says device clients are public | No | No | No |
| ID token | Yes | Yes, with `openid` | **No.** An access token only; the server calls `GET /user` with it | Yes |
| Stable identifier | `sub`: unique across Google accounts, never reused | `oid` with `tid`. **`sub` is different for each application**, so a re-registered app would change everyone's | The numeric `id`. `login` can be renamed | `iss` and `sub`. GitLab's `sub` is the user's id |
| Email | `email` may change and is not a key; `email_verified`; `hd` names the Workspace domain and must be checked in the token, since the request's `hd` parameter is only a hint | Mutable, and Microsoft says never to use it for authorization. For a single-tenant app an unverified address still appears unless the app opts out (`removeUnverifiedEmailClaim`); `xms_edov` says whether the domain is verified | Not in the token. `GET /user/emails` needs a scope and lists verified addresses | As the issuer has it. GitLab sends `email` only when the user made it public |
| Restrict to a company | The consent screen's "Internal" type admits only the organization; and check `hd` | A single-tenant registration; and check `tid` in `iss` | Check organization membership with the user's token; the organization must approve an OAuth app before it sees anything private | The issuer is the company's |
| Groups | None in the token. The Admin SDK or Cloud Identity API, with the admin's credentials | A `groups` claim of object ids, left out above 200 groups in favor of a pointer to Graph | Teams, through the API | Okta and Keycloak can add a claim. GitLab: `groups_direct` in the token |
| Token lifetime | About an hour (the sample; not stated) | One hour by default, 10 minutes to a day. "Valid until expiry" | A GitHub App's user token: 8 hours, refresh for six months. An OAuth app's: until revoked | The issuer's |
| Refresh | A refresh token that ends on revocation, six months unused, or an admin's session length | 90 days of inactivity, replaced at each use | Above | The issuer's |
| What an admin registers | A project, an Internal consent screen, one client. Minutes; no review for these scopes (inferred) | One app registration, single tenant, a `http://localhost` redirect, public client flows only if the device grant is wanted. Minutes | A GitHub App with the device flow on, installed on the organization. Minutes | One client |
| Second factor, conditions | The Workspace's sign-in policy; session length for cloud services | Conditional Access, by application, device and flow; continuous access evaluation for apps that support it | The organization's SAML sign-on must be active when the app is authorized | The issuer's |
| Suspended, or gone | New sign-ins fail. A token already issued stands until it expires. What happens to refresh tokens on suspension was found only in secondary sources | New sign-ins fail and refresh tokens are revoked; an ID token already issued stands until it expires | **Leaving an organization does not revoke a token.** The server must check membership again at each sign-in | The issuer's |

Three consequences for a design:

- **Nothing learned at sign-in is learned again until the next one.** No provider tells a relying party that a person was suspended. The sign-in's lifetime is the window, exactly as the certificate's is in §17.4, and the only faster ways are `principal disable`, a provisioning job, or renewing by refresh token every hour or so, which asks the provider again and costs a longer-lived secret on the machine.
- **Generic OpenID Connect covers four of the five.** Issuer, client id, an optional secret, the claim that is the subject (`sub`, or `oid` with `tid`), and one claim to match against an allowed value (`hd`, `tid`). GitHub is the exception and needs code that calls its API from the server with the person's token.
- **The loopback flow is not uniform either.** Google's desktop client type and Entra's public client take a loopback redirect (general knowledge). Whether a GitHub App's authorization code exchange works without its client secret was not established; GitHub's own CLI uses the device grant. So GitHub may mean the device grant in practice.

## 6. Managing users

**What an identity is keyed on.** OpenID Connect's own specification says the issuer and subject together are the only claims a relying party can rely on as a stable identifier, and that email, phone number and name must not be used as one (Core §5.7). The incidents:

- **nOAuth** (Descope, June 2023): in Entra, the `email` claim was mutable and unverified. An attacker set the address on an account in a tenant of their own to a victim's, signed in to an application that matched accounts by email, and was the victim. Microsoft's answer was never to use the claim for authorization and to stop sending unverified addresses to most applications. Two years on, a survey found 9 of 104 applications in Entra's gallery still vulnerable.
- **A lapsed domain** (Truffle Security, January 2025): whoever buys a defunct company's domain can recreate its Workspace accounts, and they present the same `email` and `hd` as the old ones. Applications keyed on those admit the new owner.
- **Auto-linking** a new provider to an existing account because the email matches and the provider says it is verified keeps appearing as an advisory in sign-in libraries, several in 2026.

So the rule proposed to the maintainer holds: key on the provider's permanent subject; email is what an admin types and what people see; two providers are never joined by matching addresses. With one correction: for Entra the subject to store is `tid` and `oid`, not `sub`.

**How this maps onto §17.2.** The handle, the display name and `kind` and `owner` stand. The external identity becomes a small table, not one string:

| Column | Holds |
|---|---|
| `name` | The handle it belongs to |
| `provider` | Which configured provider: `google`, `entra`, `github`, an issuer's name, or `authority` for a §17 certificate principal |
| `subject` | The permanent identifier: `sub`; `tid/oid`; GitHub's numeric id; the certificate principal. Empty until first sign-in |
| `invited_as` | What the admin typed: an address or a GitHub login. Shown to admins; never matched after the binding |
| `bound_at`, `updated_by`, `write_id` | Bookkeeping |

A handle may have several rows, which is how **a person with two providers** is one principal: an admin adds the second, deliberately. It lives where §17.7 puts the mapping, outside every history.

**Invitation and binding.** An admin invites `alice@example.com` at a provider. The first sign-in from that provider whose verified address equals the invitation, and whose tenant or domain is the one the server allows, binds the subject to the handle, as an event. From then on only the subject is matched: a later sign-in with the same address and a different subject is refused and reported, which is the lapsed-domain case. An invitation should expire if unused (a setting), since until it is bound it is exactly a match by email. GitHub is better here: the server can resolve a login to its numeric id when the admin types it, so nothing is ever matched by a mutable name.

**Just-in-time creation** (anyone from the allowed domain gets a principal at first sign-in) is the convenient setting and should be off by default: it makes the provider's whole directory members of the server, and decision 17 exists because who may place a principal matters. If built, a just-in-time principal belongs to no project and reaches nothing until an admin adds it.

**Changing an address** changes `invited_as` and the display, by an admin, with no trace in any project (§17.2). **Leaving:** the provider stops signing them in, and access ends within a lifetime; `principal disable`, or the job, ends it at once (decisions 20 and 21). **Server admins** stay names in the operator's config file (§17.12, decision 3 there); a server admin who signs in by sign-on should still keep a key in `authorized_keys`, since otherwise the provider's outage is theirs too. **Project roles** stay rows of the control store.

**Directory sync.** §17.8's job that runs `aries admin apply` is unchanged and still the recommendation, with the provider's API as the query: Microsoft Graph's delta queries, Google's Admin SDK, Okta's Users API, GitHub's organization and team members. SCIM pushed by the directory needs an HTTPS endpoint that the directory can reach: Entra and Okta can push to an arbitrary endpoint (Entra also through an agent on premises), and whether Google Workspace can push to one outside its catalog could not be confirmed. That endpoint is the strongest reason there will ever be for a second listener, and it is not strong enough yet. **Groups to roles** stay in the job's file, where the mapping is reviewed (§17.5): a `groups` claim read at sign-in has the costs §17.5 lists for a certificate's groups, and Entra drops the claim above 200 groups.

## 7. Agents, CI and unattended sessions

**At a desk.** A developer's one to several hundred sessions on one machine use that person's signed-in key, as they use their key today, and are told apart by session id and harness. One sign-in for each machine for each lifetime, whatever the number of sessions. The audit record is unchanged: principal, session, machine, and now the key's fingerprint joins to the sign-in event that created it.

**How long a sign-in lasts** is the real question for a hundred long-running sessions, and there are three positions:

| Lifetime | Suits | Costs |
|---|---|---|
| A working day (8 to 16 hours), connections closed at expiry as §17.4 proposes for certificates | People at their desks | Every session stops overnight. Work meant to run unattended must not run as the person |
| A week or more, as cloud CLIs do | A team that values not being interrupted | A stolen laptop is worth a week; a leaver has a week unless disabled |
| A day, renewed silently by refresh token while the provider still vouches | Long-running sessions as the person | A refresh token on disk is a credential worth 90 days at Entra; it is the long-lived secret the design set out to remove, though it is the provider's to revoke |

Recommended: a day by default, the lifetime a server setting with a ceiling, silent renewal a setting that is off by default, and unattended work pushed to identities of its own, which decision 8 already requires.

**Unattended.** An agent in CI or a cloud sandbox has no browser and should have no stored key. It has, on most platforms, a token that says what it is (read):

| Platform | Token | Names | Can the audience be chosen for each request? |
|---|---|---|---|
| GitHub Actions | An OIDC token from `token.actions.githubusercontent.com` | The repository and its owner by name and by id, the ref, the workflow, the run, the actor, the environment. Repositories created or renamed after 15 Jul 2026 get a subject built from ids | Yes |
| GitLab CI | `id_tokens` | Project, namespace, pipeline, job, ref, user | Yes, in the job |
| Google Compute Engine | An identity token from the metadata server | Project, zone, instance, service account | Yes |
| AWS | Since Nov 2025, `sts:GetWebIdentityToken`: a signed token naming the IAM principal, with an issuer for each account | The principal's ARN, the account, tags, the instance | Yes |
| Azure | A managed identity's token from the metadata service | The identity | The resource it is for |
| Kubernetes | A projected service account token | Namespace and service account | Fixed in the pod's specification |

The enrollment path takes such a token exactly as it takes a person's: `aries login --workload`, or by itself when it finds the platform's variables. The server holds, for each agent principal, a rule: the issuer, and the claims that must match (for GitHub, the repository's id and the environment, not its name). The key that results lasts the job, an hour by default. Where the platform lets the caller choose the audience, `aries` puts the server's name and the new key's fingerprint in it, which binds the token to the key as a nonce does (inferred; not tried).

**Whose identity.** §17.6 stands: a principal of its own for each unattended worker, `kind: agent`, with an owner. What a sign-in adds is that the launch is recorded without anyone writing it: the sign-in event holds the workload token's claims that matter (the run, the actor, the repository's id), so the audit stream can say that session S of principal `ci-nightly`, owned by alice, was run 8123 of a named workflow started by bob's push. Per-session identities were considered and are not recommended: a principal for each session multiplies rows without telling an auditor more than the session id and the sign-in event already do.

## 8. Deploying and maintaining it

**The journey under each serious option**, as steps added to the thirteen of §2:

| | Steps added, once | Steps for each person | Steps for each further machine | Secrets added on the server | Trust decisions added |
|---|---|---|---|---|---|
| A. Keys | 0 | 1 | **1, by an admin** | 0 | One for each key |
| B. A company's authority | 3: its public key in the config, three sshd lines, a restart. And an authority, if there is none: a service with a signing key, its own sign-on configuration and its own upgrades | 1 (`--external`) | 0 | 0 in Aries. The authority's signing key, wherever it lives | The authority, and who configures it |
| C. `aries login` | 3: register an app at the provider; a `login:` block; outbound port 443 to the provider | 1 (an invitation) | **0** (`aries login`, by the person) | 0. Google's client secret is in the config and is not a secret | The provider's tenant or domain; whether the device grant is on; the lifetime |
| D. `opkssh` | 5: install it as root, its system user, two policy files, the key command with a wrapper, outbound 443 | 1, as root on the server | 0 | 0 | The provider; `opkssh`; the wrapper |
| E. A tunnel | The broker's setup, and its client on every machine and runner | The broker's, plus the row used with it | The broker's client | The broker's | The broker |

**The first admin and break-glass** do not change under any option, and must not: a bare key in the hand-maintained `authorized_keys`, which sshd reads whatever ariesd, the provider or the authority is doing (measured again here: with the issuer stopped, that key was admitted). A server that has turned sign-on on should still be set up with that key, and the guide should say to keep it offline.

**Rotating the host key** is the weakest part of today's journey and no option touches it. `host_key` in `.aries.yaml` is one fingerprint, checked by equality (`internal/client/conn.go`), so a rotation is a commit to every repository on the same day as the server's change, with every clone refused in between. Two things would make it routine, and are separate from sign-in: accepting a list of fingerprints, so the new one is committed before the change and the old removed after; or pinning a host authority and having the server present a host certificate, so a rebuilt server needs no commit at all. The first is a few lines of the client and needs no protocol change. It is recommended whatever else is decided. *Built 14 Oct 2026, the first: `server.host_keys` ([aries.md](aries.md) §2, As built; the procedure is in [the server guide](../server.md#rotating-the-host-key)).*

**Rotating a signing key.** Under C there is none, which is a property worth as much as any other in this table. Under B it is §17.3's: two authorities listed while one is replaced. The provider's own signing keys rotate by themselves and the server follows by fetching them; what was measured with `opkssh` (every credential from before a rotation refused) does not arise under C, since a token is checked once, at enrollment, and the key it yields does not depend on the provider afterwards.

**Upgrades.** Nothing new under A, B or E. Under C, a provider that changes its flow breaks sign-in until ariesd or `aries` is upgraded, and existing sign-ins run out within a lifetime: the upgrade path becomes something a team depends on within a day, not a month. Under D, `opkssh` is a second binary to keep current, root-owned.

**A second server, or a move.** Today: copy the config, the host keys and `authorized_keys`, restore the dump (the guide's restore). Under C the app registration and the `login:` block move with the config; signed-in keys are in the control database's dump and expire anyway; the enrollment key, if derived from the host key, moves with it. A second server for the same company is a second redirect and audience on the same registration, or a second registration; each server should have its own client id, so that a token for one is not a token for the other.

**When the provider is down.** Under C: no new sign-ins, no renewals; every machine signed in keeps working until its lifetime ends; unattended agents that enroll for each job stop at once. So the lifetime is both the revocation window and how long a team survives an outage, as §17.3 says of certificates. Under D every connection needs the provider. Under B, the authority.

**Outbound network.** Today ariesd needs none in operation (telemetry and upgrades are the operator's choice). Under C the daemon fetches the provider's discovery document and key set over HTTPS: `accounts.google.com` and `www.googleapis.com`, `login.microsoftonline.com`, the company's issuer; for GitHub, `api.github.com`, called with the person's token. It should cache the key set and refetch only on an unknown key id, at a bounded rate, so that a stranger cannot make the server fetch at will. The enterprise example's server already has outbound access through NAT for upgrades.

**AppArmor.** The bridge's profile does not change: an enrollment is the same binary reaching the same socket. The daemon's profile (`ariesd-serve`) gains outbound TCP to port 443, name resolution and the system's certificate store, which it has not needed before; the unit's `RestrictAddressFamilies` already allows it. Under D, `opkssh` runs unconfined before authentication unless someone writes it a profile. Under B, nothing.

**Observability.** The audit stream (§16) already carries key additions and removals with a fingerprint. A sign-in is a key addition with more to say: the provider, the principal, the key's fingerprint and end time, the machine, and for a workload the launch. Refused sign-ins (an unknown subject, a wrong tenant, an expired or replayed token) are what a security team will want and are not control-store events, since nothing changed; they belong in the log with a stable event name and a counter, by provider and reason and never by address. `aries admin status` should show each provider with the time its keys were last fetched and whether the last fetch failed, as it shows the keys file.

**What could be automated.**

- **An installer for the server.** The thirteen steps are a script already, in prose. `install.sh --server` does step 3; one that did 1 to 6 and 8 to 13 on a fresh Ubuntu machine, printing the host key's fingerprint and a `.aries.yaml` at the end, would turn an afternoon into ten minutes and remove most of the places a step can be got wrong. It would hold no secret it does not generate.
- **`ariesd doctor`**, run on the server, checking what the guide asks a person to check: the `Match` block's lines and that no `ForceCommand` exists; the two files' owners and modes; that the keys file is fresh; the clock; that the provider's keys can be fetched; that a break-glass key exists and is not also in the store. `aries doctor` on a client: which key or sign-in would be used and when it ends, whether the host key matches, whether the server is reachable through the configured transport.
- **Infrastructure-as-code examples** for the two cloud examples the deployment guides already describe, and for the app registration at each provider, which is the step people will get wrong.

**As built (the server installer, 14 Oct 2026).** Decision 64's first half is built: `install-server.sh`, beside `install.sh`, does steps 1 to 6 and 8 to 13 of §2's journey on a fresh Ubuntu 24.04 LTS machine, in eleven steps of its own, and ends by printing the `.aries.yaml` of step 7 with the host key's fingerprint. [With the installer](../server.md#with-the-installer) in the server guide maps each of its steps to the guide's sections, which stay the reference. As this section proposed, it holds no secret it does not generate: the database password goes into the 0600 config file before Dolt is given it, Dolt's `root` password is generated and forgotten, and neither is ever an argument or printed. It holds no release key either: the binaries come through `install.sh --require-signature`. What differs from the proposal, or was decided while building it:

- **A server of many projects from the start.** The control database, the 100 granted names and the keys file are set up with the first database, so the first admin adds people and projects with `aries admin` from then on. A server of one project is still set up by hand.
- **Each step checks before it acts**, so a second run changes nothing and says so, and an interrupted run is finished by running it again. Dolt is administered as its own user from its data directory, the guide's way once `root`'s password is forgotten, which is what lets a resumed run finish the SQL.
- **It never changes a file it did not write.** A file it would write that exists with other text is shown as a difference and the run stops. sshd's drop-in is checked with `sshd -t` before a reload, and removed again if it fails.
- **The guide and the script are held together by a test** (`scripts/installserver_test.go`): the script's numbered steps equal the guide's table of them, every section the table names exists, every setup section is named, and each of the seven files the script writes is byte for byte the guide's. The Dolt pin is held to `internal/version` like its other copies.
- **Ubuntu 24.04 LTS only**, the platform the guide was run on; another system is refused with a pointer to the guide. Since 14 Oct 2026 it also accepts 26.04 LTS, once it had run there ([aries.md](aries.md), decision 23's As built for 26.04); a test holds its list to the guide's.
- **Left to the operator:** the firewall, `PasswordAuthentication`, telemetry, backups and upgrades. `ariesd doctor`, decision 64's second half, is not built.

**Said plainly.** *A small team with no platform group* needs three things: an installer, so that standing a server up is not a project; `aries login` against the provider they already have, which is GitHub or Google, so that a new laptop or a new teammate is not an admin's errand; and a list of host key pins, so that rebuilding the server is not a flag day. Until the second exists, one key for each person in a synced agent is the honest advice. *An enterprise with a platform group* needs none of the convenience and all of the control: certificates from its own authority (B) or sign-on against its tenant with the device grant off (C), a tunnel in front (E), a provisioning job, the audit stream in its SIEM, and a written account of what a sign-in's lifetime means for a leaver. It will ask for SCIM, and the answer for now is the job.

## 9. Threats

For the serious options. "Lifetime" is a sign-in's or a certificate's.

| Attacker has | A. Keys | B. Company authority | C. `aries login` | D. `opkssh` |
|---|---|---|---|---|
| **A stolen laptop** | The key, forever, unless it has a passphrase or lives in hardware | The certificate and its key, for the rest of a lifetime | The signed-in key, for the rest of a lifetime. A refresh token too, if renewal is on, until the provider revokes it | The same as C, for the policy's lifetime |
| **A stolen token or certificate** (without the private key) | n/a | Nothing: a certificate is public | An ID token bound by a nonce to a key: nothing. One from the device grant: the person, until it is used once or goes stale, so it must be accepted once, only when fresh | The ID token inside is a bearer token for that client id, at any service that shares it |
| **A malicious project member** | Their own rights. A project admin can add keys to people within reach (decision 36 shows them) | Their own rights | Their own rights. An invitation is a new thing a project admin could write for someone within reach; it should follow the reach rule and decision 36, and nobody invites an identity onto their own handle | A policy line needs root |
| **A compromised server** | Everything in the stores; it can admit any key. It learns no private key | The same; and nothing that works elsewhere | The same; plus the ID tokens it is shown, which name a client id only that server uses, and for GitHub the access tokens it is handed, which should carry no scope | The same; plus every ID token on every connection |
| **A compromised provider account** | n/a | Is that person, for as long as the authority follows the provider | Is that person at the next sign-in. The second factor and the provider's own detection are the defense | The same |
| **A compromised provider or authority** | n/a | Is anyone | Is anyone who has an identity at that provider | The same |
| **A phished sign-in** | n/a | The authority's sign-on is as phishable as any | Below | A lookalike page gains a token bound to the victim's own key: nothing, unless the attacker also runs the client |
| **A wrong clock on the server** | Nothing depends on it but leases | §17.14 | Below | The policy's lifetime is measured on the server's clock: set back, an old certificate works again |

**Device-code phishing.** The attacker starts a device authorization against the real client id, sends the victim the real verification page and the code, and receives the victim's token when they approve. Nothing on the page is false, and a second factor does not help, since the victim completes it. Microsoft documented a campaign of this (Storm-2372, from August 2024, later tied to a Russian state actor), the FBI warned in May 2026 of a phishing kit sold for it, and the same has been shown against GitHub's device flow. The guidance (read): block the device grant by Conditional Access wherever it is not needed (Microsoft; CISA's baseline for Entra says it should be blocked; a Microsoft-managed policy does so in tenants that do not use it); where it is needed, limit it to known devices and networks; teach people to check which application the page names; revoke refresh tokens on suspicion. RFC 10027, the cross-device security practice published in August 2026, lists the device grant as susceptible and recommends, in order of strength, a same-device or FIDO flow in its place, proof of proximity, short-lived codes and telling the user what they are authorizing.

For Aries this means:

- **Default to the loopback flow.** A phishing page cannot receive a redirect to the victim's own `localhost`, and with the nonce the token is useless for any key but the one `aries` made.
- **The device grant is a server setting, off by default,** and where the provider can restrict it to an application and a network the guide says how. A token from it is accepted once, only within a few minutes of its `iat`, on the store's clock.
- **A sign-in can only ever enroll for an invited, bound identity.** A phished token from a stranger's tenant matches nothing.
- **For a headless machine, prefer not to need the device grant at all**: sign in on a machine with a browser and have that session approve the headless machine's pending key, shown as a code and the key's fingerprint on both. This is also a cross-device flow and can also be socially engineered, but the approval is a command typed into `aries`, naming a fingerprint, by someone who has just signed in, and no provider needs to allow anything. It is a design question for the spike, not a finding.
- **Every sign-in is announced** to the principal's inbox and the audit stream, with the machine and the provider, so a sign-in the person did not make is seen.

One threat is specific to Aries's users: **an agent on a developer's machine can drive that developer's browser**, where the provider's session is already open. Then `aries login` completes with nobody present, and decision 19's protection (an agent holding a key cannot give itself another) is only as good as the provider's demand for a fresh second factor. Where it matters, the provider should require re-authentication for this application (Entra's sign-in frequency; Google's session length), and the guide should say so. This was reasoned, not tested.

**The clock** (§17.14 and decision 37). Under C three clocks appear and, as there, only the server's decides. The provider's writes `iat` and `exp` into a signed token. The person's decides nothing. The server's is read twice: when the daemon checks the token (freshness, and not before `iat`), and when a key's end time passes. Both should be read on the store's time, which does not go backward. sshd reads `expiry-time` on the wall clock, so a clock set back would revive an expired line at sshd; but ariesd stops writing a line when the store's time passes its end, so within a tick the line is gone and there is nothing for sshd to revive, and the daemon's check at the handshake refuses the key meanwhile. This is stronger than a certificate, which the client keeps and can present again to an sshd whose clock went back. A clock that jumps forward ends sign-ins early: an outage, and the safe direction. A token replayed inside its freshness window is stopped by accepting each token once, which needs the daemon to remember what it has seen for as long as the window, bounded (rule 18).

**What revocation achieves, and how fast.**

| Act | Takes effect | Because |
|---|---|---|
| `principal disable`, `member remove`, `key remove` | At once, connections closed and claims released | Decisions 20 and 21; unchanged by any option |
| Removing one signed-in key | At once | It is a key in the store |
| The provider suspends the person | At the next sign-in: within a lifetime | Nobody tells the server. Faster only with the job, or renewal |
| The provider revokes a refresh token | At the next renewal | The same |
| A company authority revokes a certificate | Never, at sshd (§17.1); at once with `aries admin cert revoke` | §17.4 |
| Removing an `opkssh` policy line | At the next connection; open connections stay | sshd checks once |

## 10. The claims checked

What the maintainer was told before this research, and what holds.

| Claim | Verdict |
|---|---|
| Google offers a device flow and OpenID Connect ID tokens | **Holds** (read) |
| Google's device flow needs a client secret shipped in the client | **Holds** (read). Google says of installed applications that such a secret is not treated as one; that page does not name the device client type. The secret can come from the server at enrollment and need not ship in `aries` |
| Google's device flow limits scopes | **Holds** (read). The limit does not matter: `openid`, `email` and `profile` are allowed |
| Microsoft Entra offers a device flow and ID tokens | **Holds, and understates the problem.** Microsoft recommends blocking the flow and ships a managed policy that does. A design that depends on it will not work in many enterprise tenants |
| GitHub offers a device flow but only an access token, so a server would call GitHub's API with the user's token | **Holds** (read). Added: leaving an organization does not revoke the token, so membership is checked at each sign-in; a GitHub App's user tokens expire in 8 hours where an OAuth app's do not |
| Identity keyed on issuer plus the provider's permanent subject | **Holds, with a correction.** For Entra, `sub` is different for each application registration: store `tid` and `oid`. For GitHub, the numeric id |
| Email only as what an admin types and what is shown; never auto-link two providers by email | **Holds** (read: OpenID Connect Core §5.7, nOAuth, the lapsed-domain report) |
| The server signs the key into a short-lived certificate with an authority key of its own | **Unnecessary.** An expiring key line does the same with no signing key (measured) |
| The hard part is that nothing reaches the server before sshd authenticates, so a pre-authentication path is needed | **True for that flow, and not hard.** Three paths were measured working; one needs no sshd change. And OpenPubkey shows a flow that needs none, at other costs |
| (Implied) the device flow is the way to sign in from a terminal | **Wrong as a default.** It is the phishable flow, it cannot bind a token to a key, and it is the one enterprises are told to block. Loopback with PKCE first |

## 11. Decisions for the maintainer

**Decided 14 Oct 2026.** The maintainer took the recommended answer to each of the fourteen questions below; they are decisions 52 to 65 of [aries.md](aries.md) §14. The order of work in §1 stands, with the spikes of "What to spike first" before any design of sign-in is accepted, and the host key list (11) and the installer (13) free to start now.


Each with a recommended answer and the cost of each choice.

1. **Does Aries sign people in itself (C), beside keys and beside §17's certificates?** Recommended: yes, as a third way in that a server turns on, never required. *Cost of yes:* a stage of work; a token verifier and outbound requests in the daemon; a path strangers reach; providers to keep up with. *Cost of no:* small teams keep hand-managed keys, or run `opkssh` beside Aries; the enterprise answer is B alone.
2. **Expiring keys, or certificates signed by Aries?** Recommended: expiring keys. *Cost:* the expiry is enforced by a file being rewritten and by the daemon, not by a signed document, so it depends on ariesd running, which every sign-in already does. *Cost of certificates:* a signing key on the server to guard and rotate, the client changes of §17.9 as a prerequisite, and a credential the client can keep and replay to a clock set back.
3. **Which pre-authentication path?** Recommended: spike the published enrollment key first, the any-key command second. *Cost of the first:* a known private key to explain, and proof of possession done in Aries's own exchange. *Cost of the second:* `AuthorizedKeysCommand` returns to the `Match` block, with a root-owned script and sshd's log full of accepted strangers. *Cost of neither* (an HTTPS endpoint): decision 9 reversed.
4. **Is the device grant offered at all?** Recommended: yes, as a server setting that is off by default, with tokens accepted once and only fresh. *Cost of on by default:* the phishing exposure of §9, and a default that enterprises must turn off. *Cost of never:* a headless machine needs the approve-from-another-machine flow, or a key.
5. **Which providers first?** Recommended: generic OpenID Connect (which is Google, Entra, Okta, Keycloak and GitLab by configuration), then workload tokens, then GitHub. *Cost of GitHub first:* it is the one that needs its own code and may need the device grant, built before the common path exists. *Cost of GitHub last:* the smallest teams wait longest.
6. **Invitation only, or just-in-time creation from an allowed domain?** Recommended: invitation only; just-in-time as a later setting that creates principals in no project. *Cost of invitation only:* an admin step for each person, once. *Cost of just-in-time:* the provider's directory becomes the server's.
7. **How long does a sign-in last, and may it renew silently?** Recommended: a day, a server setting with a ceiling; renewal by refresh token a setting, off by default. *Cost:* sessions as a person stop within a day, so unattended work needs agent identities (decision 8), which is the intent. *Cost of a week:* the windows of §9 are a week. *Cost of renewal on:* a 90-day credential on each machine.
8. **Does the daemon close a connection when its signed-in key ends?** Recommended: yes, as §17.12's decision 8 recommends for certificates, and decided together with it. *Cost:* long sessions reconnect or stop. *Cost of no:* a session outlives its sign-in without limit.
9. **Is the external identity a table of (provider, subject, invited address)?** Recommended: yes, deciding it before I2 is built, since I2 as written has one string. *Cost:* a slightly larger I2. *Cost of one string:* a migration when the second provider arrives, and no home for the invited address.
10. **May a person approve their own new machine from one already signed in?** Recommended: only from a session that has just signed in at the provider, never by a key alone; decide after the spike. *Cost of yes:* a cross-device approval of Aries's own to defend. *Cost of no:* headless machines need the device grant or an admin.
11. **Is the host key pin made a list now?** Recommended: yes, independent of everything else. *Cost:* a small client change and a line of the guide. *Cost of not:* every rotation stays a flag day.
12. **Is `opkssh` documented as a supported way in?** Recommended: not yet; try it privately (spike 4) and decide after. *Cost of supporting it:* a wrapper to maintain, a key command back in the guide, and a 0.x dependency in the security model. *Cost of not:* nothing, for anyone who does not ask.
13. **An installer and a `doctor` before or after sign-in?** Recommended: the installer before. It helps every option and every new user, and it is the cheaper half of "easy to deploy". *Cost:* a script to keep in step with the guide, which a test can check as `scripts/` already checks the gate.
14. **Do tunnels other than IAP get first-class support?** Recommended: on request, each as IAP was done; document Tailscale and WireGuard as network layers now. *Cost of each:* a helper invocation to validate and test. *Cost of a general proxy command:* code execution from a cloned repository, already ruled out.

## 12. Not verified

- **Anything against a real provider.** No Google Workspace, Entra, Okta, GitLab or GitHub tenant, application or account was used. Every row of §5 is documentation. In particular: that Google's desktop and Entra's public clients complete a loopback flow with PKCE and return the nonce; whether Google's device grant returns a nonce; whether a fresh Entra tenant already blocks the device grant; whether a GitHub App's code exchange works with PKCE and no secret; what each provider does to tokens when a person is suspended; Google's ID token lifetime; the scope needed for GitHub's membership check.
- **Any workload token** (GitHub Actions, GitLab CI, a cloud instance, Kubernetes), and the idea of putting a key's fingerprint in a chosen audience.
- **ariesd and `aries` in any of this.** A script stood in for the forced command. `aries` was not run with an `opkssh` certificate, from a file or from an agent; §17.1 measured it with an ordinary certificate from an agent only.
- **The published-key path** as such. What was measured is a forced command on a key line (§15.2, long established) and the any-key command; that a derived, well-known key behaves like any other key is inferred.
- **`expiry-time`** on any OpenSSH but 9.6p1, in any form but fourteen digits on a server whose zone is UTC, and at the scale of thousands of lines.
- **OpenSSH other than 9.6p1** as the server for anything here. Release notes for 9.8 to 10.6 were read and show no change to `AuthorizedKeysCommand`; 10.3 says a certificate with no principals never matches, which does not affect `opkssh`'s one principal.
- **AppArmor, systemd and SELinux** for every proposal. Docker Desktop's kernel loads none. What §8 says of the profiles is read from them.
- **A security review of `opkssh`.** None was found; its paper and one critical analysis were seen only as search summaries. Its origin with Cloudflare likewise.
- **The no-authentication account under a hardened PAM configuration**, and whether the any-key command meets sshd's `PerSourcePenalties` (9.8 and later).
- **The brokers of §4.5**, all from documentation: in particular that IAM, Entra and tailnet identities do not reach sshd through the tunnels, the sshd configuration OS Login and Entra login install, certificate lifetimes at Cloudflare and Entra, and every price.
- **Whether Google Workspace can push SCIM to an endpoint outside its catalog**; third-party guides disagree.
- **Agents driving a browser to complete a sign-in**, and what each provider's re-authentication settings do about it.
- **Costs to build** are estimates from the size of §17's slices, not from a breakdown.
- Several items the research could cite only from a search result or from memory are marked in the text ("search summary", "inferred", "from memory"). The sub-claims most worth rereading at the source before a design relies on them: Microsoft's removal of unverified email claims and its exception for single-tenant apps; the managed policy's rollout rule; Teleport's license terms; AWS's outbound identity federation.

## 13. Sources

Read on 13 Oct 2026 unless marked. Provider pages change; the dates matter.

**Standards**
- RFC 8628, OAuth 2.0 Device Authorization Grant: <https://www.rfc-editor.org/rfc/rfc8628>
- RFC 9700, OAuth 2.0 Security Best Current Practice: <https://www.rfc-editor.org/rfc/rfc9700>
- RFC 10027, cross-device flows security (seen at the datatracker as published from this draft): <https://datatracker.ietf.org/doc/draft-ietf-oauth-cross-device-security/>
- OpenID Connect Core, §5.7 Claim Stability and Uniqueness: <https://openid.net/specs/openid-connect-core-1_0.html#ClaimStability>
- RFC 7643 and RFC 7644, SCIM (not fetched): <https://www.rfc-editor.org/rfc/rfc7643>, <https://www.rfc-editor.org/rfc/rfc7644>

**OpenSSH**
- `sshd_config(5)`: <https://man.openbsd.org/sshd_config>; `sshd(8)`, the `authorized_keys` format: <https://man.openbsd.org/sshd>; `ssh-keygen(1)`: <https://man.openbsd.org/ssh-keygen>; `ssh-add(1)`: <https://man.openbsd.org/ssh-add>; `ssh_config(5)`: <https://man.openbsd.org/ssh_config>
- Release notes 8.2, 8.4, 8.9, 9.8, 10.0 and the index: <https://www.openssh.org/txt/release-8.2>, <https://www.openssh.org/txt/release-8.4>, <https://www.openssh.org/txt/release-8.9>, <https://www.openssh.org/txt/release-9.8>, <https://www.openssh.org/txt/release-10.0>, <https://www.openssh.org/releasenotes.html>
- Go's SSH packages: <https://pkg.go.dev/golang.org/x/crypto/ssh>, <https://pkg.go.dev/golang.org/x/crypto/ssh/agent>

**OpenPubkey**
- `opkssh`: <https://github.com/openpubkey/opkssh> (README; releases and repository figures from GitHub's API)
- The paper: <https://eprint.iacr.org/2023/296> (search summary). A critical analysis: <https://arxiv.org/pdf/2307.16607> (search summary)
- Dex: <https://github.com/dexidp/dex>

**Google**
- Device flow: <https://developers.google.com/identity/protocols/oauth2/limited-input-device>; sign-in on devices: <https://developers.google.com/identity/gsi/web/guides/devices>
- OpenID Connect and its claims: <https://developers.google.com/identity/openid-connect/openid-connect>; OAuth overview (client secrets, refresh tokens): <https://developers.google.com/identity/protocols/oauth2>
- Consent screen audiences: <https://support.google.com/cloud/answer/15549945>
- IAP TCP forwarding: <https://docs.cloud.google.com/iap/docs/using-tcp-forwarding>; OS Login and its certificates: <https://docs.cloud.google.com/compute/docs/oslogin>, <https://docs.cloud.google.com/compute/docs/oslogin/certificates>
- Instance identity: <https://docs.cloud.google.com/compute/docs/instances/verifying-instance-identity>

**Microsoft**
- Device code flow: <https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code>
- ID token claims: <https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference>; optional claims: <https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference>
- Token lifetimes and refresh tokens: <https://learn.microsoft.com/en-us/entra/identity-platform/configurable-token-lifetimes>, <https://learn.microsoft.com/en-us/entra/identity-platform/refresh-tokens>
- Conditional Access, authentication flows, and managed policies: <https://learn.microsoft.com/en-us/entra/identity/conditional-access/concept-authentication-flows>, <https://learn.microsoft.com/en-us/entra/identity/conditional-access/managed-policies>
- Unverified email claims (search summary): <https://learn.microsoft.com/en-us/graph/applications-authenticationbehaviors>
- Entra login for Linux: <https://learn.microsoft.com/en-us/entra/identity/devices/howto-vm-sign-in-azure-ad-linux>; Bastion's native client: <https://learn.microsoft.com/en-us/azure/bastion/native-client>
- SCIM provisioning and Graph delta queries: <https://learn.microsoft.com/en-us/entra/identity/app-provisioning/on-premises-scim-provisioning>, <https://learn.microsoft.com/en-us/graph/delta-query-overview>

**GitHub**
- Device flow: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps>; refreshing a GitHub App's user tokens: <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens>
- Users, emails, organization members: <https://docs.github.com/en/rest/users/users>, <https://docs.github.com/en/rest/users/emails>, <https://docs.github.com/en/rest/orgs/members>
- OAuth app restrictions, token expiry and revocation: <https://docs.github.com/en/organizations/managing-oauth-access-to-your-organizations-data/about-oauth-app-access-restrictions>, <https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/token-expiration-and-revocation>
- Actions OIDC: <https://docs.github.com/en/actions/reference/security/oidc>

**Other providers and platforms**
- Okta: <https://developer.okta.com/docs/guides/device-authorization-grant/main/>, <https://developer.okta.com/docs/concepts/scim/>
- GitLab: <https://docs.gitlab.com/api/oauth2/>, <https://docs.gitlab.com/integration/openid_connect_provider/>, <https://docs.gitlab.com/ci/secrets/id_token_authentication/>
- AWS: <https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-getting-started-enable-ssh-connections.html>, <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/connect-linux-inst-eic.html>, <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/connect-with-ec2-instance-connect-endpoint.html>, <https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_providers_outbound.html>
- Kubernetes projected tokens: <https://kubernetes.io/docs/concepts/storage/projected-volumes/>
- Tailscale: <https://tailscale.com/kb/1193/tailscale-ssh>, <https://tailscale.com/kb/1080/cli>, <https://tailscale.com/kb/1244/tsnet>, <https://tailscale.com/pricing>
- Cloudflare: <https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/ssh/ssh-infrastructure-access/>, <https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/short-lived-certificates-legacy/>
- Teleport: <https://goteleport.com/docs/enroll-resources/server-access/openssh/openssh-agentless/>, <https://goteleport.com/docs/feature-matrix/>, <https://goteleport.com/docs/reference/machine-workload-identity/configuration/>
- step-ca: <https://github.com/smallstep/certificates>, <https://smallstep.com/docs/step-ca/provisioners/>, <https://smallstep.com/docs/step-cli/reference/ssh/login/>
- Vault and OpenBao: <https://developer.hashicorp.com/vault/docs/secrets/ssh/signed-ssh-certificates>, <https://www.hashicorp.com/en/license-faq>, <https://openbao.org/docs/secrets/ssh/signed-ssh-certificates/>
- 1Password's agent: <https://www.1password.dev/ssh/agent/>; Secretive: <https://github.com/maxgoedjen/secretive>

**Incidents and guidance**
- nOAuth, Descope, 20 Jun 2023: <https://www.descope.com/blog/post/noauth>; Microsoft's response (search summary): <https://msrc.microsoft.com/blog/2023/06/potential-risk-of-privilege-escalation-in-azure-ad-applications>; the 2025 survey (search summary): <https://www.semperis.com/blog/noauth-abuse-alert-full-account-takeover/>
- The lapsed-domain report, Truffle Security, 13 Jan 2025: <https://trufflesecurity.com/blog/millions-at-risk-due-to-google-s-oauth-flaw>
- Storm-2372 device-code phishing, Microsoft, 13 Feb 2025, updated 31 Jul 2026: <https://www.microsoft.com/en-us/security/blog/2025/02/13/storm-2372-conducts-device-code-phishing-campaign/>
- FBI public service announcement, 21 May 2026: <https://ic3.gov/PSA/2026/PSA260521>
- CISA's secure configuration baseline for Entra ID, policy MS.AAD.3.9v1: <https://raw.githubusercontent.com/cisagov/ScubaGear/main/PowerShell/ScubaGear/baselines/aad.md>
- Device-code phishing against GitHub, Praetorian, June 2025 (search summary): <https://www.praetorian.com/blog/introducing-github-device-code-phishing/>
