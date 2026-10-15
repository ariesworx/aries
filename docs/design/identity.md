# Identity for v1.0.0: sign-in, expiring keys and service principals

15 Oct 2026. **Status: design, proposed. Nothing in it is built, and it decides nothing the maintainer has not decided;** its open questions are in [§15](#15-decisions-for-the-maintainer). It takes the recommendation of [access-research.md](access-research.md) (option C: Aries signs people in itself, by recording an expiring key in the file sshd reads), decisions 52 to 65 and decision 83 ([aries.md](aries.md) §14), and turns them into a plan that can be built for v1.0.0, in slices with a gate each. It was widened the same day, at the maintainer's request, to a sign-in that finishes in a browser on another machine ([§6.3](#63-sign-in-by-url-when-no-browser-can-be-opened)) and a Claude Desktop extension ([§9](#9-the-claude-desktop-extension)).

**Why a document of its own.** [aries.md](aries.md) is 2,250 lines and §17 already holds one identity design (certificates, handles, provisioning) that is proposed and not decided. This design is the one decision 83 puts in v1.0.0; it reuses §17's model (the handle, display name, kind and owner of aries.md §17.2, and aries.md §17.14's clocks) and supersedes what §17 and access-research.md left open about sign-in. §17 links here. Where the two differ, this document is the plan for v1.0.0 and §17 stays the design for certificates (its I3 and I4), which v1.0.0 does not include.

**Evidence.** The spike in [`spike/identity`](../../spike/identity/RESULTS.md) ran the sign-in end to end on 15 Oct 2026 against Keycloak 26.8.1 and two real sshd (OpenSSH 9.6p1 on Ubuntu 24.04 and 10.2p1 on 26.04), in throwaway containers. What it measured is marked **Measured**; what comes from a provider's documentation is **Read**; the rest is design. Nothing was run against Google, Entra ID, Okta, GitHub or a cloud's workload identity: the checklist for those real-tenant spikes is in [RESULTS.md, section 7](../../spike/identity/RESULTS.md#7-the-real-tenant-spikes-what-the-maintainer-registers).

**Where it lands.** After the Postgres port's slices P1 and P2 (decision 79), so its tables are written once, in the new migration sets, in Postgres's dialect, with no `write_id` (decision 79 removes rule 6's column). Nothing here changes `internal/store`'s SQL or migrations before then.

## 1. Summary

A person signs in with `aries login`. `aries` reaches the server over SSH by a path open before anyone is known ([§5](#5-the-way-in-before-sign-in)), makes a new key pair, and opens the provider's sign-in page in a browser: on the same machine with a loopback redirect, or anywhere at all by printing a URL and taking back the address the browser lands on ([§6.3](#63-sign-in-by-url-when-no-browser-can-be-opened)). The server checks the provider's ID token, whose nonce commits to the new key and the server's challenge, finds the person by (issuer, subject), and records the public key with an end time a day later. ariesd writes that key to the file sshd reads, with `expiry-time` on its line, and from then on `aries` signs in with it as with any key. Nobody adds a key for a new machine, a person who signs in holds no private key worth more than a day, and Aries holds no signing key.

A service principal (a CI job, an unattended agent, a cloud workload) does the same with the token its platform already gives it, and a trust rule that matches the token's claims names the principal. CI's stored key goes away.

**The model, in one table each.**

| Thing | What it is |
|---|---|
| Principal | As today, plus `kind` (`person` or `service`), a display name and, for a service, an `owner` |
| Identity | An external identity of a person: provider, issuer, subject, and the address the admin invited. One person may have several |
| Invitation | An identity with no subject yet. The first sign-in whose verified address matches binds it |
| Expiring key | A key row with an end time and an origin (`admin`, `login`, `workload`) |
| Trust rule | A service principal, a workload provider, and claims that must match |
| Provider | An issuer the operator configured, in `ariesd.yaml`, never over the protocol |

**Slices** ([§14](#14-plan)), after P1 and P2, about 11 to 12 weeks of one engineer in all:

| Slice | Delivers | Estimate |
|---|---|---|
| N0 | The real-tenant spikes and the way-in spike under AppArmor (decision 54) | 1 week, with the maintainer |
| N1 | Identity: `kind`, display name, owner, identities and invitations, the admin commands | 1.5 weeks |
| N2 | Expiring keys: the end time, `expiry-time` in UTC, the handshake's check, pruning, connections closed at expiry | 1 week |
| N3 | The way in and the verifier: the enrollment path, the exchange, the token check, provider settings, outbound HTTPS | 2 weeks |
| N4 | `aries login` for people: loopback, the URL path, key storage and choice, `whoami`, refusals | 1.5 weeks |
| N5 | Service principals: trust rules; GitHub Actions, Google Cloud and Azure tokens; `aries login --workload`; `docs/ci.md` | 1.5 weeks |
| N6 | GitHub's adapter for people, and the device grant as a server option | 1 week |
| N7 | The Claude Desktop extension | 1 week, and the maintainer's signing certificate |
| N8 | The guides, the Docker test server with Keycloak, the threat model, a security review of the whole | 1.5 weeks |

## 2. What stays as it is

- **SSH is the only transport** (decision 9). ariesd opens no new port, and the provider never calls the server.
- **sshd authenticates, and reads one file the daemon writes** (decisions 22 and 26). A signed-in key is a line of that file like any other; it only carries an end.
- **The store decides.** Every line names its key's fingerprint, and the handshake refuses a key the store does not hold for that principal, or holds with an end that has passed. A stale line gets past sshd and no further.
- **The first admin and break-glass** are a bare key in the hand-maintained `authorized_keys`, which works whatever ariesd or the provider is doing. A server admin who signs in by sign-on keeps one such key, offline.
- **Nobody adds a credential to their own account** (decisions 19 and 38). A sign-in is the exception decision 52 made: the provider's sign-on, with its second factor, is the approval decision 19 asks another admin for, and it can only enroll for an identity an admin invited.
- **Decisions 20 and 21.** Disabled wins; losing access releases claims at once. Unchanged by how the principal signed in.
- **Rule 12.** Nothing of this is an MCP tool. Sign-in is a person's act; inviting, trust rules and providers are admin commands, CLI only.
- **Rule 19.** Every time the server compares (a token's `iat`, `exp` and `auth_time`, a key's end) is the store's time, which does not go backward.
- **A server of one project** gets none of it: identities, invitations and trust rules live in the control store, so sign-in needs a server of many projects. The installer sets one up.

## 3. Tables

All in the control schema, written through `Control.write` in one transaction with an event each (rules 5 and 7), in the first Postgres control migration that follows P2. Names and types are Postgres's; `text` columns carry the length checks the store already applies to names.

### 3.1 `principals`, widened

| Column | Type | Holds |
|---|---|---|
| `kind` | `text NOT NULL DEFAULT 'person'`, `CHECK (kind IN ('person','service'))` | A service principal signs in only by a workload token or a key an admin added, never by a person's sign-in |
| `display_name` | `text NULL`, at most 128 characters, safetext | What people see beside the handle (aries.md §17.2). Not unique |
| `owner` | `text NULL REFERENCES principals(name)` | The person who answers for a service principal; required for `service`, empty for `person` |

The display name sits on the row, not in a table of its own as aries.md §17.7 proposed: that table existed to escape Dolt's history (`dolt_ignore`), and Postgres keeps none. The event that changes it names the field and never its value (aries.md §17.2). aries.md §17.12's question 6 is moot after decision 79.

### 3.2 `identities` (decision 60)

| Column | Type | Holds |
|---|---|---|
| `id` | `bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY` | |
| `principal` | `text NOT NULL REFERENCES principals(name)` | A `person` |
| `provider` | `text NOT NULL` | The name of a provider in `login.providers` (`google`, `entra`, `okta`, `keycloak`, `github`) |
| `issuer` | `text NOT NULL` | The issuer the token named, exactly: `https://accounts.google.com`; Entra's `https://login.microsoftonline.com/TENANT/v2.0`; GitHub's adapter writes `https://github.com` |
| `subject` | `text NULL` | The permanent identifier, by the provider's `subject` setting: `sub`; Entra's `oid` (its `sub` differs per app registration, Read); GitHub's numeric user id. NULL until bound |
| `invited_as` | `text NOT NULL` | What the admin typed: an address, or a GitHub login, which the server resolves to the numeric id at once and binds without waiting ([§6.6](#66-github)) |
| `invited_by`, `invited_at` | `text`, `timestamptz` | |
| `invite_until` | `timestamptz NULL` | When an unbound invitation lapses (`login.invite_ttl`, 7 days). NULL once bound |
| `bound_at` | `timestamptz NULL` | The first sign-in |
| `last_sign_in` | `timestamptz NULL` | Bookkeeping, written with `wtx.touch` (rule 7): history is the `key.signin` event |
| `rev` | `bigint NOT NULL` | |

- `UNIQUE (issuer, subject) WHERE subject IS NOT NULL`: one person per external identity.
- `UNIQUE (provider, lower(invited_as)) WHERE subject IS NULL`: one open invitation per address and provider.
- At most `identities_per_principal` (8) rows per principal.
- An identity is bound **only** by a sign-in whose token says the address is verified (`email_verified` true, and for Entra the domain verified, `xms_edov`, Read), whose tenant or domain is the provider's `require`, and whose address equals `invited_as` case-insensitively. From then on only (issuer, subject) is matched: a later token with the same address and another subject is refused and logged (the lapsed-domain case, access-research.md §6). Two providers are never joined by address.

### 3.3 `principal_keys`, widened

| Column | Type | Holds |
|---|---|---|
| `expires_at` | `timestamptz NULL` | The end. NULL for a key with none (today's keys). Indexed, for pruning and for the next expiry |
| `origin` | `text NOT NULL DEFAULT 'admin'`, `CHECK (origin IN ('admin','import','login','workload'))` | Who put it there |
| `identity` | `bigint NULL REFERENCES identities(id)` | For `login` |
| `trust_rule` | `text NULL REFERENCES trust_rules(name)` | For `workload` |
| `machine` | `text NULL`, at most 64 characters, safetext | What the client said it is, for `login`: shown to the person and admins, trusted for nothing |

- A `login` key has an `identity` and an `expires_at`; a `workload` key a `trust_rule` and an `expires_at` (`CHECK`s say so).
- A new sign-in from the same principal and `machine` replaces that machine's earlier `login` key in the same write, so one person's keys do not pile up; past that, `login_keys_per_principal` (16) unexpired login keys.
- `key add --expires DURATION` gives an admin's key an end too, for a contractor's or a stopgap CI key (access-research.md §1, step 4).

### 3.4 `trust_rules`

| Column | Type | Holds |
|---|---|---|
| `name` | `text PRIMARY KEY` | The principal-name pattern; unique on the server |
| `principal` | `text NOT NULL REFERENCES principals(name)` | A `service` |
| `provider` | `text NOT NULL` | The name of a provider in `workload.providers` |
| `claims` | `jsonb NOT NULL` | An object of claim name to exact string value, 1 to 16 entries |
| `key_lifetime` | `interval NOT NULL` | How long the key a match gets lasts; at most `workload.max_key_lifetime` |
| `state` | `text NOT NULL` | `active` or `disabled` |
| `created_by`, `created_at`, `updated_at`, `rev` | | |

At most `trust_rules_per_principal` (16) and `trust_rules` (1,000) on the server.

### 3.5 Not tables

- **Providers** are the operator's, in `ariesd.yaml` ([§10](#10-settings-limits-and-refusals)), like aries.md §17.3's certificate authorities: a provider is a way in for anyone it vouches for, so naming one is root's act, never an admin's over the protocol.
- **Challenges, and tokens already used,** are the daemon's memory, bounded ([§10](#10-settings-limits-and-refusals)). A restart forgets them, which costs a person a second `aries login`. A token that is not bound to a key (the device grant, GitHub, an Azure workload) is accepted only if issued after the daemon started, so a restart cannot reopen a replay window.
- **Sign-ins** are events, not rows: `key.signin` records the provider, the identity's id, the fingerprint, the end, the machine and, for a workload, the rule and the launch's claims ([§7.5](#75-what-the-audit-stream-says-of-a-launch)).

### 3.6 Events

| Op | Target | After state |
|---|---|---|
| `principal.set` | name | The names of the fields set (never a display name's value) |
| `identity.invite` | principal | Provider, identity id, invitation's end; not the address |
| `identity.bind` | principal | Provider, issuer, identity id |
| `identity.remove` | principal | Identity id, and the keys removed with it |
| `key.signin` | fingerprint | Principal, origin, provider or rule, identity id, end, machine, flow (`loopback`, `url`, `device`, `workload`) |
| `key.remove` | fingerprint | As today; reason `expired` when pruned is not an event (below) |
| `trust.add`, `trust.set`, `trust.remove` | rule name | Principal, provider, the claim names (values too: they are ids, not personal data) |

Deleting a key whose end has passed is pruning, which rule 7 allows quietly (`wtx.quiet`): its end was history when it was written.

## 4. Expiring keys

### 4.1 The line

```text
restrict,expiry-time="20261016193113Z",command="/usr/local/bin/ariesd stdio --principal alice --key SHA256:…" ssh-ed25519 AAAA…
```

**Always in UTC, marked `Z`.** This answers decision 54's question about the zone. Measured on OpenSSH 9.6p1 (24.04) and 10.2p1 (26.04), in three zones, with the server's clock correct: a value marked `Z` meant the same instant in every zone; fourteen digits with no `Z` were read in the server's own zone. On a server set to UTC−11, a line written in UTC digits one minute in the past **was admitted**: an expired key revived for eleven hours. On UTC+14 a line three minutes ahead was refused at once. So a daemon that left the `Z` off would be correct only on servers set to UTC, which the guide's servers usually are and nothing enforces ([RESULTS.md, section 4](../../spike/identity/RESULTS.md#4-sshd-and-expiry-time)).

The value is the key's `expires_at` truncated to the second. sshd compares it with the wall clock at authentication, and only then: a connection admitted a minute before its line's end was still receiving output after it, on both versions (Measured), as aries.md §17.1 found for certificates.

### 4.2 Who enforces what

| When | Who | Clock |
|---|---|---|
| A new connection, at authentication | sshd, by the line | The wall clock |
| The same connection, at the handshake | The daemon: the store must hold the key for the principal, and the store's now must be before `expires_at` | The store's (rule 19) |
| An open connection, at the end | The daemon closes it (decision 59) | A monotonic timer set at the handshake for `expires_at`, and a check of the store's time on each request; the earlier closes it (aries.md §17.14) |
| The line itself | ariesd stops writing it at the first write after the end | The store's |

The store's time is the later of the wall clock and its high-water mark (decision 37), so it is never behind the wall clock, and the daemon refuses at or before the moment sshd would. A clock set back revives a line at sshd (the wall clock), and the daemon still refuses: that is aries.md §17.14's argument, which holds here unchanged and is stronger than a certificate's, since nothing the client keeps can be presented again once the line is gone.

### 4.3 Writing the file on time

Today the file is written after each key change and on the 30-second tick. With ends, the daemon also keeps one timer for the earliest `expires_at` in the index and rewrites the file when it fires, so a line is gone within a second of its end, not within a tick. Pruning deletes rows whose end is more than `login.prune_after` (1 hour) past, so a refused connection's log and `key list` still show the key for a while; it runs on the reaper's tick, quietly.

### 4.4 Connections at the end

The daemon closes a connection whose key has ended with a close frame naming the reason (`auth`, "your sign-in ended at T", fix: `aries login`). Claims are untouched: a lease the client no longer renews ends as leases always have. `aries mcp` reconnects and is refused, and then does what [§9.4](#94-signing-in-without-a-terminal) says. Ten minutes before the end, `prime`, `whoami` and the MCP server's notices carry one line saying when the sign-in ends; the welcome already carries what they need ([§12](#12-protocol)).

## 5. The way in before sign-in

A machine nobody knows must hand the server a token before sshd knows its key. Decision 54 chose to spike two candidates under AppArmor, the published enrollment key first. This design is written so that the daemon's side is the same for both, and only sshd's configuration and the bridge's invocation differ; slice N0 runs both on the guide's server and the maintainer chooses.

### 5.1 The published enrollment key (first candidate)

ariesd writes one more line at the head of its keys file, after the comment:

```text
restrict,command="/usr/local/bin/ariesd stdio --enroll" ssh-ed25519 AAAA…ENROLL
```

The key pair is derived, not stored: its seed is SHA-256 of `aries-enroll-v1`, a zero byte and the server's host key fingerprint. `aries` computes the same pair from the fingerprint it already pins (`host_key` or each of `host_keys`), and ariesd from the host public keys it reads at start (`/etc/ssh/ssh_host_*_key.pub`, world-readable; a setting, `enroll.host_keys`, names them where they live elsewhere). So nothing about it is in `.aries.yaml`, nothing is a secret, and a rotated host key brings a new line with it. Its private half is public knowledge by construction, which a scanner or an auditor will ask about, as about Vagrant's; the guide says what it can reach.

What a stranger reaches through it: the bridge, as the `aries` account, with no terminal and no forwarding, and through it the daemon's enrollment handler and nothing else. sshd logs `Accepted publickey for aries … ED25519 SHA256:…` with the enrollment key's own fingerprint, so its connections are told apart in the log.

### 5.2 The key command for enrollment (second candidate)

`AuthorizedKeysCommand` returns to the `Match` block, a root-owned script of a few lines that prints, for any key, `restrict,command="/usr/local/bin/ariesd stdio --enroll --key %f"`. Measured in access-research.md §3: the files are consulted first, so a known key never reaches it. It proves possession for free, and undoes `AuthorizedKeysCommand none`, which keeps a cloud image's own command away from the account; every stranger is logged as an accepted public key.

### 5.3 The exchange

Whichever path, the bridge runs with `--enroll`, puts `enroll: true` and the client's address (from `SSH_CONNECTION`) in the bridge frame, and the daemon answers only the enrollment ops on that connection. It takes no connection slot and opens no store. The exchange carries its own proof of possession, so the first candidate loses nothing to the second:

1. **`enroll.begin`** → the providers the server offers people, each with its kind, issuer, client id, scopes, the flows on (`loopback`, `url`, `device`), and any hint (`hd`, `login_hint`'s domain); the workload audience; a challenge (32 random bytes, single use, 10 minutes); and the server's time.
2. The client makes its key pair and computes the nonce: base64url of SHA-256 of `aries-login-v1`, 0, the challenge, 0, and the new public key's wire form. It runs the flow the person chose (§6) and gets back an authorization code.
3. **`enroll.finish`** → the provider's name, the flow, the code, the PKCE verifier and the redirect URI (or, for a workload, the platform's token); the new public key; the machine's name; and a signature by the new key over `aries-enroll-v1`, the challenge, the host key fingerprint and the hash of the rest. The server checks the signature, spends the challenge, **exchanges the code itself** at the provider's token endpoint (§6.4), verifies the ID token, finds or binds the identity, writes the key with its end, rewrites the file, and answers with the principal, the fingerprint and the end. The client then connects as itself, with the new key.

Each refusal is a `proto.Error` with a code and a fix ([§10.3](#103-refusals)), sent before the connection closes.

### 5.4 Bounds

Strangers reach the handler, and sshd's `MaxStartups` and `PerSourcePenalties` no longer bound a caller who authenticates successfully, so the daemon bounds it: at most `enroll.conns` (32) enrollment connections at once, `enroll.rate` per source address (6 a minute, burst 10) and in total (120 a minute), `enroll.challenges` outstanding (1,000), an exchange's whole life 15 minutes (the device grant's poll included), and 64 KiB for a frame. Past any, `busy` with a fix to retry in a minute. Refusals are counted by provider and reason, never by address, and logged at debug, sampled, by address (aries.md §16.4's rules).

## 6. `aries login`

### 6.1 Flows, and which is the default

| Flow | When | Binds the token to the key | Provider must allow |
|---|---|---|---|
| **Loopback**: authorization code, PKCE (S256), state, nonce, redirect to `http://127.0.0.1:PORT/callback` where `aries` listens | A browser can be opened on this machine. The default | Yes, by the nonce | A loopback redirect on any port (RFC 8252 §7.3) |
| **URL**: the same request; `aries` prints the URL, the person opens it in any browser, and pastes back the address the browser lands on | No browser here: over SSH, in a container, on a headless machine. On by default ([§6.3](#63-sign-in-by-url-when-no-browser-can-be-opened)) | Yes, by the nonce | The same; nothing more |
| **Device grant** (RFC 8628) | A server option, off by default (decision 55) | Only where the provider returns the nonce: Keycloak did (Measured), Dex did not (access-research.md §3) | The device grant, which Entra's managed policy and CISA's baseline block (Read) |

The authorization request also carries `max_age` (`login.fresh_auth`, 10 minutes) and, where the provider honors it, `prompt=login`, so the person proves themselves at the provider each time ([§6.7](#67-a-day-and-no-silent-renewal)).

**Measured** against Keycloak with a stand-in browser: loopback, the URL path with the address pasted back, and the provider's own code page all completed with PKCE and the nonce; the ID token came back signed ES256 with the nonce equal, `auth_time` present and `email_verified` true. The same token presented with another key, or again with a new challenge, was refused (the nonce no longer matched); a code exchanged with the wrong PKCE verifier was refused by Keycloak (`PKCE verification failed`); a person with no invitation was refused by the server. The URL `aries login` prints was about 370 characters; the address pasted back about 265. The device grant completed as well, with a 9-character code; Keycloak returned the nonce sent with the device request and refused a second poll with the same device code.

### 6.2 What `aries login` does

1. Connects by the enrollment path (§5) and gets the server's providers. With one provider for people, it uses it; with several, `--provider NAME`, or it lists them and asks. Nothing about a provider is in `.aries.yaml`: the pinned host key vouches for the server's answer.
2. Chooses the flow: `--url` or `--device` if given; otherwise loopback if a browser can be opened (macOS and Windows always; Linux with `DISPLAY` or `WAYLAND_DISPLAY` set and `SSH_CONNECTION` not), otherwise the URL path.
3. Runs it ([§6.3](#63-sign-in-by-url-when-no-browser-can-be-opened) for the URL path), sends `enroll.finish`, and stores the key ([§6.8](#68-where-the-key-lives-and-which-key-aries-offers)).
4. Prints the principal, the provider, the machine name and when the sign-in ends, and exits 0. `--json` prints one document.

### 6.3 Sign in by URL when no browser can be opened

The maintainer asked that `aries login` work where no browser can be opened, by the person opening a printed URL in any browser and finishing there. Three ways to do that were compared.

| | Device grant (RFC 8628) | **Code with PKCE, the address pasted back** | A page that shows the code ("open this link, then paste the code") |
|---|---|---|---|
| What the person does | Opens a short URL anywhere, types an 8- or 9-character code, signs in; the terminal goes on by itself | Opens the printed URL anywhere, signs in; the browser lands on a `127.0.0.1` address it cannot open; copies that address into the terminal | Opens the URL, signs in, copies the code the page shows into the terminal |
| Where the page lives | The provider's | Nowhere: the redirect is to loopback | The provider's own code page where it has one (Entra's `nativeclient`, Keycloak's `urn:ietf:wg:oauth:2.0:oob`; Google removed its out-of-band page in 2022, Read), or one Aries serves, which needs an HTTPS listener (decision 9 rules it out) or a static page on a host someone runs |
| The token commits to the new key | **Not by the standard.** RFC 8628 defines no nonce: Dex ignored one (access-research.md §3), Keycloak returned it (Measured, [RESULTS.md, section 2](../../spike/identity/RESULTS.md#2-people-signing-in)). Even bound, the key is whoever started the grant's, which is the phisher's | Yes | Yes |
| The code alone is worth | A bearer token, to whoever started the grant | Nothing without the PKCE verifier, which never leaves the machine that started | The same |
| Provider policy | Blocked in many enterprise tenants: Entra's managed Conditional Access policy, CISA's baseline (Read) | It is the authorization code grant: what a tenant allows for loopback, it allows here | The same, where the page exists |
| Phishing | The attacker starts a grant and sends the victim the real page and a code. Documented campaigns (Storm-2372; the FBI, May 2026) | The attacker starts the flow and must persuade the victim to send back the address their browser shows. Possible, and an unusual ask; a lookalike page cannot receive the redirect | As the middle column |
| Works from a phone | Yes, easily | Awkwardly: copying a 265-character address on a phone | Yes, with a 98-character code (Keycloak, Measured) |
| Same-machine fallback | n/a | `aries` listens on the port while it waits, so if the browser is on the same machine, or the port is forwarded (`ssh -L`), it completes with no paste | n/a |

**Recommended: the code with PKCE and the address pasted back is the URL path, on by default, and decision 55 stands unchanged.** It is the same grant, the same registration and the same verification as the loopback flow, so a server that offers loopback offers it with nothing more to configure at the provider; it keeps the nonce's binding; and a tenant that blocks the device grant does not block it. Where the provider has its own code page (Entra, Keycloak), `--url` uses it and the person pastes a short code instead, since the code is as safe as the address. The device grant stays a server setting, off by default, for a browser on a phone where pasting back is impractical: a token from it is accepted once, only within `login.device_fresh` (2 minutes) of its `iat`, its nonce is checked where the provider returns one (Keycloak did), and the server announces the sign-in to the principal's inbox.

What `aries login --url` prints, as designed:

```text
Open this URL in a browser on any machine and sign in to Example Workspace (google):

  https://accounts.google.com/o/oauth2/v2/auth?response_type=code&client_id=…

Your browser will then fail to open a page at http://127.0.0.1:53111/…
That is expected. Copy the whole address from its address bar and paste it here:
>
```

`aries` checks the pasted address's `state` before it sends anything, and refuses an address from another sign-in with a fix to paste the one from this one.

### 6.4 The token check

**The server exchanges the code.** The client sends the code, the PKCE verifier and the redirect URI over the pinned SSH channel, and ariesd calls the provider's token endpoint. So the ID token never exists on the client, a provider whose desktop client needs a client secret (Google's, Read) or whose web flow does (GitHub's, to be established) keeps it on the server in a 0600 file, and the one code path serves GitHub too. The spike exchanged the code on the client; only which process sends the POST differs. The alternative, the client exchanging and sending the ID token, is [§15](#15-decisions-for-the-maintainer)'s question 3.

**One verifier for every OpenID Connect provider** (`spike/identity/oidc`, Measured against Keycloak; table-tested against a fake issuer):

- Settings pin, per provider: the issuer (exact string), the audience (the client id), the claim that is the subject, and `require` (claims that must equal given values: `hd` for Google, `tid` for Entra).
- The discovery document's `issuer` must equal the setting exactly, and every endpoint must be `https`.
- Signing keys come from `jwks_uri`, are cached, and are fetched again only for an unknown key id, at most once a minute (`login.jwks_refetch`), so a stranger with made-up key ids cannot make the server fetch at will (`TestRefetchIsBounded`). At most 32 keys and 64 KiB; a key marked `"use":"enc"` is never a signing key (Keycloak publishes one, Measured).
- Algorithms RS256 and ES256 only; `none`, the HMAC family and any `crit` header refused; RSA under 2,048 bits refused; the header's algorithm must agree with the key's type.
- `iss` exact; `aud` contains the client id; with several audiences, `azp` equals it; `exp` not past and `iat` not ahead, each with `login.skew` (30 seconds); `iat` within `login.max_token_age` (10 minutes); `nonce` equal to the one the server recomputes from its challenge and the key; `auth_time` within `login.fresh_auth`.
- All on the store's time (rule 19).

**Outbound access.** For the first time, ariesd makes requests to the internet: each provider's discovery document and keys, its token endpoint, and for GitHub `api.github.com`. Each request is HTTPS, with a 10-second timeout and a size cap, no redirect followed to another host, and the system's certificate authorities. What it changes:

- *AppArmor.* `ariesd-serve` gains `network inet dgram`, `network inet6 dgram` (DNS) and `include <abstractions/ssl_certs>`, which its comments already name as what a hosted endpoint needs. The bridge's profile does not change: enrollment is the same binary reaching the same socket. Recommended unconditionally, since the rules are read-only or already implied by `network inet stream`; the alternative is a `local/` include the guide and the installer add when `login:` is set.
- *systemd.* The unit has `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` and no `IPAddressDeny`, so nothing changes. A guide that adds an egress allowlist names the providers' hosts.
- *The installer.* No new step: `login:` is the operator's to write. It prints, at the end, that sign-in needs outbound HTTPS to the provider.
- *A server without outbound access* (a private network with no NAT) cannot offer sign-in; keys keep working.
- *`aries admin status`* shows each provider: when its keys were last fetched, how many, and whether the last fetch failed.

**Each provider's quirks**, by configuration (Read unless marked; N0 checks them):

| Provider | Issuer | Subject | `require` | Quirks |
|---|---|---|---|---|
| Google Workspace | `https://accounts.google.com` | `sub` | `hd: example.com` (in the token; the request's `hd` is only a hint) | The desktop client's code exchange needs its client secret, which Google says is not treated as one; the server holds it. No `prompt=login`: `max_age` is what forces re-authentication, and whether Google honors it for a Workspace session is for N0 |
| Microsoft Entra ID | `https://login.microsoftonline.com/TENANT/v2.0` (single tenant) | `oid` (`sub` differs per app registration) | `tid` | A public client ("mobile and desktop", `http://localhost`); `prompt=login` honored; unverified addresses appear for single-tenant apps unless the app opts out, so binding also needs `xms_edov`; `groups` not read |
| Okta | `https://example.okta.com/oauth2/default` or the org's | `sub` | none (the issuer is the company's) | A native app with PKCE; loopback port rules and `max_age` to be checked |
| Keycloak | `https://HOST/realms/REALM` | `sub` | none | **Measured:** loopback on any port with `http://127.0.0.1/*` registered; the client's PKCE requirement also applies to its device grant ("Missing parameter: code_challenge_method" without one); ES256 by a client setting; with an open browser session a second sign-in completed with no page shown unless `prompt=login` was sent |
| GitHub | n/a: no ID token | numeric id | the organization | [§6.6](#66-github) |

### 6.5 Invitations (decision 57)

An admin invites; the person signs in; the sign-in binds.

```text
aries admin principal add alice --display "Alice Example" --invite alice@example.com --provider google [--project P]
aries admin invite add alice bob@example.com --provider entra      # a second identity, deliberately
aries admin invite list [--pending]
aries admin invite remove ID
aries admin identity list alice | remove ID
```

Who may invite is the rule for keys (aries.md §15.3): a server admin, or a project admin with reach over the principal; nobody invites an identity onto their own principal (decision 19's reasoning: an agent holding someone's key could otherwise point their principal at an identity it controls). An invitation lapses after `login.invite_ttl` (7 days) unused, since until it is bound it is a match by address. Just-in-time creation is not built (decision 57).

Removing an identity removes the keys it enrolled, closes their connections, and is reported like `key remove`. Disabling the principal does what it does today and ends every key.

### 6.6 GitHub

GitHub gives people no ID token (Read). The adapter is a GitHub App with no permissions: its user tokens identify the person and reach nothing.

- The flow is the same loopback or URL flow with PKCE against `https://github.com/login/oauth/authorize`; there is no nonce. The server exchanges the code with the App's client secret (a 0600 file on the server, like the database password), calls `GET /user` for the numeric id and login, checks membership with `GET /user/memberships/orgs/ORG` (`state: active`), and **revokes the token** (`DELETE /applications/CLIENT_ID/token`) before it answers, so the token is good for nothing afterward.
- **The binding** is weaker than OpenID Connect's: the code is bound to the PKCE verifier, which only the machine that started holds, and the server spends it at once; a code intercepted at the redirect is useless without the verifier. What GitHub cannot give is a signed statement that the sign-in was for this key. The design accepts that: the code never leaves the pinned channel.
- **Invitations** are by login: the server resolves it to the numeric id when the admin types it (`GET /users/LOGIN`), so the identity is bound at once and nothing is ever matched by a name that can be renamed.
- **Leaving the organization** does not revoke a GitHub token (Read); membership is checked at each sign-in, so access ends within a day.
- **No forced re-authentication.** GitHub has no `prompt=login` or `max_age`; with a session open, the authorize page completes by itself after the first time (general knowledge; N0 checks). So a GitHub sign-in is as fresh as the person's GitHub session, which the organization's SAML policy may bound. The guide says so; decision 61's approval of a new machine is not offered for GitHub ([§6.9](#69-approving-a-new-machine-decision-61)).
- To establish in N0: whether a GitHub App's code exchange accepts PKCE, whether it needs the secret, and whether a loopback redirect may take any port.

### 6.7 A day, and no silent renewal

A signed-in key lasts `login.lifetime`, 24 hours by default, at most `login.max_lifetime` (7 days), from the moment the server writes it (decision 58). Nothing renews it: there is no refresh token on any machine, and `aries` never asks the provider for one (`offline_access` is not requested).

**A finding that matters here** (Measured): with the browser's session at the provider still open, a second sign-in within `max_age` completed with **no page shown** to the person, and its `auth_time` was that of the first. So "no silent renewal" is not kept by storing no refresh token alone: a tool that opened the URL in the person's browser could renew the day with nobody looking, an agent included (access-research.md §9). With `prompt=login` the provider showed its sign-in page again and `auth_time` moved to the new sign-in. Therefore:

- `aries login` sends `prompt=login` where the provider honors it (Entra, Okta, Keycloak) and `max_age` everywhere, and the server checks `auth_time` against `login.fresh_auth` (10 minutes). A token whose `auth_time` is older is refused with a fix to sign in again.
- For Google and GitHub, which have no `prompt=login`, freshness is the provider's session policy; the guide tells the operator how to set the session length for the app (Google's session control; GitHub's organization SAML).

### 6.8 Where the key lives, and which key `aries` offers

The private key is made on the machine and never leaves it. It is stored in the user's data directory (`internal/offline`'s, or `ARIES_DATA_DIR`), under `signin/SERVER/`, where SERVER is the hex of SHA-256 of the pinned host key fingerprints: `key` (OpenSSH format, ed25519, mode 0600 in a directory `internal/offline` checks is the user's own; on Windows, where `internal/offline` checks nothing and access is the profile directory's ACL, the same as its outbox, which N4 must look at again for a credential) and `key.json` (principal, provider, fingerprint, end). A sign-in is per server, not per project, so every project on that server, and every `aries` and `aries mcp` of the user, uses it.

Decision 35's order becomes: `ARIES_KEY_FILE`, then `key:`, then **the signed-in key for this server if it has not ended**, then the default identities and ssh-agent. A signed-in key is offered alone, as a configured key is. An ended one is not offered; if nothing else gets in, the refusal says when the sign-in ended and to run `aries login`. `aries logout` deletes the local files and asks the server to remove the key (decision 38 lets anyone remove a key of their own).

A macOS Keychain or Windows DPAPI store is possible later; a file is what `aries` already trusts for `ARIES_KEY_FILE`, and its value is bounded by a day.

### 6.9 Approving a new machine (decision 61)

`aries login` on any machine that can reach the provider is already a fresh sign-in, so a person's new machine needs nobody's approval. The case decision 61 left is a machine that can reach neither a browser nor a phone. For it: `aries login --approve` on the headless machine prints a short code and its new key's fingerprint and waits; on a machine where the person can sign in, `aries login --approve CODE` runs a fresh sign-in (`prompt=login`, `auth_time` within 2 minutes) and the server enrolls the waiting key for the same principal, showing the fingerprint on both. Never by a key alone. It is an N4 stretch, small once the exchange exists, and not offered for GitHub, whose freshness the server cannot see. Recommended to build only if N0's tenants block both the URL path and the device grant somewhere real ([§15](#15-decisions-for-the-maintainer), question 6).

## 7. Service principals

### 7.1 What one is, and its rights

A principal of `kind: service`, with an `owner`, the person who answers for it (decision 8; aries.md §17.6). It is created like any principal and joined to projects as a member:

```text
aries admin principal add ci-example --service --owner alice [--project P]
aries admin trust add ci-example-main --principal ci-example --provider github-actions \
    --claim repository_owner_id=111111111 --claim repository_id=123456789 --claim ref=refs/heads/main [--key-lifetime 1h]
```

- **Rights.** A member's, as the CI gate needs today ([docs/ci.md](../ci.md#what-this-costs)): reporting a ci gate's check from the session `ci` (decision 67), creating and updating issues. A service principal is never a project admin, never a server admin, and never passes a human gate (decision 68 names people). Decision 8's narrower default rights are its own work and attach to `kind`.
- **Who may create one, and add rules.** As for keys: a server admin, or a project admin with reach over it. Nobody adds a trust rule for a principal they are (decision 19). The owner may be anyone the creator may name; changing the owner is an admin's act.
- **Disabling** a service principal ends its keys and closes its connections, as for anyone (decisions 20 and 21).

### 7.2 Trust rules: how a token maps to a principal

A rule names a workload provider (the operator's, by name), a service principal, and claims that must each equal a string. A token is verified by the same verifier as a person's (§6.4), with the workload provider's issuer and the server's workload audience, and gets the principal of the **one** rule whose claims it satisfies. No rule: refused. Rules for two principals both matching: refused, since the server must not choose (the spike's table test; against Keycloak, the rule for the token's repository matched and one for another repository alone did not).

A rule must pin the platform's **required claims**, which are immutable ids, never names that can be renamed or recreated, and never a bare `sub`:

| Platform (`kind`) | Issuer | Required claims | Narrowing claims (any) | Never alone |
|---|---|---|---|---|
| GitHub Actions (`github-actions`) | `https://token.actions.githubusercontent.com` (an enterprise's customized issuer, or GHES's, by setting) | `repository_owner_id`, `repository_id` | `ref`, `environment`, `job_workflow_ref`, `workflow_ref`, `event_name`, `runner_environment` | `sub` and `repository`: built from names until GitHub's id-based subjects (repositories created after 15 Jul 2026, Read) |
| Google Cloud service account (`google`) | `https://accounts.google.com` | `sub` (the account's numeric unique id, never reused) and `email` | `google.compute_engine.project_id` and the like, with `format=full` | `email` alone: a deleted account's address can be recreated |
| Azure managed identity or service principal (`azure`) | `https://login.microsoftonline.com/TENANT/v2.0` (or v1's `https://sts.windows.net/TENANT/`, by the app's token version) | `tid`, `oid` | `azp` (the identity's client id), `xms_mirid` | `appid` alone |
| Kubernetes (`kubernetes`, optional) | The cluster's service account issuer | `kubernetes.io.namespace` and `kubernetes.io.serviceaccount.uid` | `kubernetes.io.serviceaccount.name` | `sub` alone |

The required claims are the verifier's, per kind, not the admin's to forget: `trust add` refuses a rule without them, and refuses patterns and empty values (a rule is matched exactly). **Kubernetes is optional and not in v1.0.0's tested set:** the generic verifier would take a cluster whose issuer publishes its discovery and keys, but nothing in N0 provides one; it is offered as configuration once someone runs it. AWS's outbound identity federation (`sts:GetWebIdentityToken`, Read) is left for 1.x.

### 7.3 Each platform's token

| Platform | How the job gets a token | Audience | Binding to the key |
|---|---|---|---|
| GitHub Actions | `permissions: id-token: write`; `GET $ACTIONS_ID_TOKEN_REQUEST_URL&audience=AUD` with `$ACTIONS_ID_TOKEN_REQUEST_TOKEN` | Chosen per request | **Yes**: `aries` asks for audience `AUD/n/NONCE`, and the server expects exactly that (inferred from the documentation; N0 checks it) |
| Google Cloud | The metadata server, `…/service-accounts/default/identity?audience=AUD&format=full`, on Compute Engine, GKE, Cloud Run; or `generateIdToken` by impersonation | Chosen per request | **Yes**, the same way (to check) |
| Azure | A managed identity from IMDS (`…/metadata/identity/oauth2/token?resource=APP_ID_URI`), or a service principal by client credentials for `APP_ID_URI/.default`, best with a federated credential rather than a secret | Fixed: an Entra app registration that stands for the Aries server | **No**: accepted once, and only within `workload.fresh` (2 minutes) of `iat` |

Azure needs one more thing from the operator: an app registration representing the Aries server, whose Application ID URI is the audience. A token for Microsoft Graph cannot be verified by anyone but Graph (Read), so the token must be for that app. The provider setting then sets `audience` to it.

### 7.4 The key it gets, and `aries` in CI

The key lasts the rule's `key_lifetime`: 1 hour by default, at most `workload.max_key_lifetime` (6 hours, GitHub's longest hosted job). It is an ordinary expiring key with `origin: workload`.

```yaml
# .github/workflows/ci.yml, a job that reports to Aries
permissions:
  id-token: write
  contents: read
steps:
  - uses: actions/checkout@<sha> # pinned
  - run: aries login --workload github-actions
  - run: aries gate check …      # as docs/ci.md says today
```

`aries login --workload KIND` (or `ARIES_WORKLOAD=KIND`) fetches the platform's token, enrolls, and stores the key in the data directory, which on a runner is `$RUNNER_TEMP` (`ARIES_DATA_DIR`, set by the same step), so the job's later `aries` commands use it and nothing outlives the runner. **No secret is stored anywhere:** `docs/ci.md`'s `ARIES_CI_PRIVATE_KEY` and its "What this costs" paragraph on a stored key are replaced by the trust rule; a stored key stays documented for CI systems with no workload identity (a key with `--expires`).

### 7.5 What the audit stream says of a launch

`key.signin` for a workload records the rule and an allowlist of the token's claims that say which launch it was: for GitHub `run_id`, `run_attempt`, `workflow_ref`, `actor_id`, `event_name`, `ref`; for Google the project and instance; for Azure the resource id. So "session S of `ci-example`, owned by alice, was run 8123 of `ci.yml` started by bob's push" (access-research.md §7) is one join from the session's events to the key's sign-in. These are ids and names of the company's own repositories; no token is stored.

## 8. Agents at a desk

An agent run by a person uses that person's signed-in key, as it uses their key today, and is told apart by session. One sign-in per machine per day serves any number of sessions. When the day ends, `aries mcp` sessions are closed and refused until the person signs in again ([§9.4](#94-signing-in-without-a-terminal)); unattended work belongs to service principals, which is the intent of decision 58.

## 9. The Claude Desktop extension

v1.0.0 ships a Claude Desktop extension: an MCP bundle (`.mcpb`), the format formerly called DXT, specified in the [`modelcontextprotocol/mcpb` repository](https://github.com/modelcontextprotocol/mcpb) ([MANIFEST.md](https://github.com/modelcontextprotocol/mcpb/blob/70fe3b34cd6dff1b3bba046638edc72a6467a4fb/MANIFEST.md), manifest version 0.3, and [CLI.md](https://github.com/modelcontextprotocol/mcpb/blob/70fe3b34cd6dff1b3bba046638edc72a6467a4fb/CLI.md), read at commit `70fe3b3` of 22 Apr 2026). A bundle is a zip archive of a local MCP server and a `manifest.json`; Claude for macOS and Windows installs it with one click and asks for the settings it declares (Read).

### 9.1 What it holds

```text
aries.mcpb (zip)
├── manifest.json
├── server/aries          macOS, a universal binary (arm64 and amd64)
├── server/aries.exe      Windows, amd64
└── icon.png
```

```json
{
  "manifest_version": "0.3",
  "name": "aries",
  "display_name": "Aries",
  "version": "1.0.0",
  "description": "Issues, memory and coordination for AI coding agents, on your team's Aries server",
  "author": { "name": "Ariesworx LLC" },
  "license": "AGPL-3.0-only",
  "server": {
    "type": "binary",
    "entry_point": "server/aries",
    "mcp_config": {
      "command": "${__dirname}/server/aries",
      "args": ["mcp"],
      "env": {
        "ARIES_HARNESS": "claude-desktop",
        "ARIES_EXTENSION": "1",
        "ARIES_PROJECT_DIR": "${user_config.project_dir}",
        "ARIES_SERVER": "${user_config.server}",
        "ARIES_HOST_KEY": "${user_config.host_key}",
        "ARIES_PROJECT": "${user_config.project}",
        "ARIES_KEY_FILE": "${user_config.key_file}"
      },
      "platform_overrides": { "win32": { "command": "${__dirname}/server/aries.exe" } }
    }
  },
  "compatibility": { "platforms": ["darwin", "win32"] },
  "user_config": {
    "project_dir": { "type": "directory", "title": "Project folder", "description": "A clone with .aries.yaml. Leave empty to give the server and project below" },
    "server": { "type": "string", "title": "Server", "description": "host or host:port, if no project folder" },
    "host_key": { "type": "string", "title": "Server's host key", "description": "SHA256:… as your admin gave it" },
    "project": { "type": "string", "title": "Project", "description": "The project's name or id" },
    "key_file": { "type": "file", "title": "SSH key (optional)", "description": "Only if your team uses keys rather than sign-in" }
  },
  "tools_generated": false
}
```

- **The binary** is the release's `aries`, unchanged: `aries mcp` with what `aries setup claude-desktop` already passes (`-C`, `ARIES_HARNESS`), read from variables, since an extension's settings reach the server as variables or arguments and an empty optional setting must mean "unset". The new variables (`ARIES_PROJECT_DIR`, `ARIES_SERVER`, `ARIES_HOST_KEY`, `ARIES_PROJECT`, `ARIES_EXTENSION`) join the hardening pass's review of public names. `ARIES_SERVER` with `ARIES_HOST_KEY` and `ARIES_PROJECT` stand for a `.aries.yaml` for someone with no clone; a project folder wins when both are given.
- **Universal macOS binary.** The manifest cannot choose by architecture, only by platform, so the macOS binary is a fat Mach-O of the two release builds. Go does not produce one; a small tool of the repository's (`internal/tools/universal`, the standard library's `debug/macho` and a 4 KB header) does, on CI's Linux runner, and its test checks that both slices are the release's bytes. Windows ships amd64, which Windows on Arm runs emulated. About 24 MB and 13 MB uncompressed (the release builds measured 11.9 MB for darwin/arm64 and 13.0 MB for windows/amd64), so a bundle of about 15 to 20 MB compressed.
- **No secret in `user_config`.** Nothing is marked `sensitive`, because nothing is a secret: the host key is public, the key file is a path.
- **Rule 12 holds.** The extension is `aries mcp`, so it exposes exactly its tools: no admin, no gate passing (decision 68 keeps passing for the session `cli`), no sign-in tool. `aries upgrade` refuses to run with `ARIES_EXTENSION` set and says to update the extension in Claude Desktop, since swapping a binary inside a signed bundle Claude Desktop manages would break both.

### 9.2 One project per entry, or one entry switching projects

**Recommended: one project per extension, switched in its settings by the person.** An extension is installed once, so it is one entry (inferred from the format, which has one `name` and one set of settings; Claude Desktop's behavior with a second copy is not established). The alternatives were one entry serving several projects, with a `project` argument on every tool, or several entries. A project argument costs schema tokens on every tool, against decision 76's budget of about 3,000; it lets an agent move between projects inside one session, which today's session ids and audit assume does not happen; and nothing in MCP tells the server which project a conversation is about. So the person picks the project in the extension's settings, and Claude Desktop restarts the server when they change. A person who works in several projects at once keeps using `aries setup claude-desktop` for the others, which writes one entry per project against an `aries` they installed. Question 9 of §15.

### 9.3 How it relates to `aries setup claude-desktop`

They are two ways to the same `aries mcp`. `aries setup claude-desktop` (`internal/agentsetup/desktop.go`, [docs/agents.md](../agents.md#claude-desktop)) writes an entry per project into `claude_desktop_config.json`, against an `aries` the person installed and upgrades with `aries upgrade`; it needs a terminal and a clone. The extension needs neither, and is upgraded by installing a newer bundle. Both may be present; setup's entries are named `aries-DIRECTORY` and the extension `aries`, so they do not collide. `aries setup claude-desktop --check` says whether the extension is also installed, if Claude Desktop records that somewhere `aries` can read without a private API (not established; [§16](#16-not-established)).

### 9.4 Signing in without a terminal

The extension's `aries mcp` runs on the person's own desktop, so it can do the loopback flow itself: a browser can be opened there.

1. At start, and on each reconnect, `aries mcp` looks for a signed-in key for the server (§6.8). With one that has not ended, it connects as now.
2. With none, or an ended one, and with the server offering sign-in, it begins `aries login`'s loopback flow in its own process: it connects by the enrollment path, listens on `127.0.0.1`, and asks the person to open the URL, in the first of these the client supports:
   - **MCP's URL elicitation** (`elicitation/create` with `mode: "url"`, in the 2025-11-25 specification, Read): the client shows the URL's host, asks the person's consent, and opens it. The URL never passes through the model. Whether Claude Desktop supports this mode is not established.
   - **Opening the browser itself**, with the operating system's opener (`open` on macOS; `rundll32 url.dll,FileProtocolHandler` on Windows), as `aries login` does.
   - **Telling the agent**: each tool call while the sign-in waits answers with one line, "Sign-in needed: a browser tab was opened to sign in to Example Workspace; if it did not open, ask the person to open URL", within the 2,000-token budget. The URL carries no secret (the PKCE challenge, the state and the nonce are useless without the verifier, which stays in the process).
3. When the redirect reaches the listener, it finishes the exchange, stores the key where `aries login` would, and connects. The sign-in is shared with the person's own terminal, both ways.
4. **Renewal without silent renewal** (decision 58). Ten minutes before the end, the MCP server's notice and `prime` say when the sign-in ends. At the end the daemon closes the connection (decision 59), and the next tool call begins step 2 again, with `prompt=login`, so the person must sign in at the provider; a browser session alone does not renew it (§6.7). No refresh token is kept. `aries mcp` opens at most one sign-in at a time and at most `login.desktop_opens` (4) browser tabs an hour, so an agent calling tools in a loop cannot fill the screen with tabs.

The private key lives where `aries login` keeps it (§6.8), in the person's data directory, not inside the extension's directory, so it survives an update of the extension and is shared with the terminal.

### 9.5 Signing and shipping

- The release workflow builds the bundle from the release's own binaries, after their signature and checksums, and lists it in `checksums.txt`, which the existing release key signs (`checksums.txt.sig`) (rule 3: by the maintainer's gated `release` environment). `install.sh` ignores it.
- The bundle is also signed in the MCPB format (`mcpb sign`: a PKCS#7 signature appended after the zip, with an X.509 certificate that should carry the code-signing extended key usage, CLI.md, Read). That certificate and its key are a **new secret of the `release` environment**, obtained and stored by the maintainer; no agent creates, sees or handles it, and the step that uses it is the maintainer's to add, as the release key's are. Whether Claude Desktop shows a signed bundle differently from an unsigned or self-signed one is not established, and decides whether the certificate is worth its cost before a directory listing is sought.
- **macOS and Windows code signing** of the binaries inside is a separate question: Claude Desktop extracts the bundle itself, and whether Gatekeeper or SmartScreen then inspects the binary is not established. Release binaries are not code-signed today.
- Each release attaches `aries-VERSION.mcpb`. A listing in Claude's extension directory is a later, separate step.

### 9.6 The slice

N7, about a week, after N4: the manifest and its test (the variables it sets are the ones `aries mcp` reads; the tool list matches `internal/mcpserver`'s), `aries mcp`'s variables, the sign-in inside `aries mcp` with the tab limit, the universal-binary tool and its test, the release steps but the MCPB signature (and `internal/tools/releaseindex`'s static tree, which must carry the new asset), the guide's section in `docs/agents.md`, and a manual check on a Mac and a Windows machine with Claude Desktop. The maintainer adds the signing step.

## 10. Settings, limits and refusals

### 10.1 Settings

In `ariesd.yaml`, read at start, refused at start when malformed or past a ceiling. A secret is a path to a 0600 file, never a value (rule 2).

```yaml
login:                          # people; absent: no sign-in
  lifetime: 24h                 # a signed-in key's life (decision 58); at most max_lifetime
  max_lifetime: 168h
  fresh_auth: 10m               # auth_time must be this recent; max_age sent
  max_token_age: 10m            # iat no older
  skew: 30s
  invite_ttl: 168h
  device_grant: false           # decision 55
  device_fresh: 2m
  prune_after: 1h
  jwks_refetch: 1m
  desktop_opens: 4              # browser tabs an hour from aries mcp
  enroll:
    path: published_key         # or key_command (decision 54, after N0)
    host_keys: [/etc/ssh/ssh_host_ed25519_key.pub]
  providers:
    - name: google
      kind: oidc
      issuer: https://accounts.google.com
      client_id: 000000000000-example.apps.googleusercontent.com
      client_secret_file: /etc/aries/google-client-secret
      subject: sub
      require: { hd: example.com }
    - name: entra
      kind: oidc
      issuer: https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0
      client_id: 00000000-0000-0000-0000-000000000000
      subject: oid
      require: { tid: 00000000-0000-0000-0000-000000000000 }
      prompt_login: true
    - name: github
      kind: github
      client_id: Iv23example
      client_secret_file: /etc/aries/github-client-secret
      org: example
workload:                       # service principals; absent: none
  audience: aries.example.com   # what a workload asks its platform for
  max_key_lifetime: 6h
  fresh: 2m                     # for platforms that cannot bind the key
  providers:
    - name: github-actions
      kind: github-actions
      issuer: https://token.actions.githubusercontent.com
    - name: gcp
      kind: google
      issuer: https://accounts.google.com
    - name: azure
      kind: azure
      issuer: https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0
      audience: api://aries.example.com
```

### 10.2 Limits (rule 18)

Each with a default, a `limits:` key, a table-driven test and a row in `docs/server.md`'s Limits table.

| Limit | Default | Bounds |
|---|---|---|
| `enroll_conns` | 32 | Enrollment connections at once |
| `enroll_rate`, `enroll_burst` | 6 a minute, 10, per source address | Strangers at the way in |
| `enroll_rate_total` | 120 a minute | All of them |
| `enroll_challenges` | 1,000 | Outstanding challenges |
| `seen_tokens` | 10,000 | Unbound tokens remembered for their window |
| `identities_per_principal` | 8 | Identities |
| `invites_open` | 1,000 on the server; 100 per inviter | Unbound invitations |
| `login_keys_per_principal` | 16 | Unexpired signed-in keys |
| `trust_rules_per_principal`, `trust_rules` | 16, 1,000 | Rules |
| `workload_keys_per_rule` | 64 | Unexpired workload keys, for a matrix of jobs |

### 10.3 Refusals (rule 11)

Each a `proto.Error`; the MCP server rewords the fix as a request to the person, never as an admin command (rule 12).

| Case | Code | Message names | Fix |
|---|---|---|---|
| No identity or invitation | `forbidden` | The provider and the address the token gave | Ask an admin of your project to invite that address: `aries admin invite add` |
| An invitation that lapsed | `forbidden` | When it lapsed | Ask for a new invitation |
| Same address, another subject | `auth` | That the account is not the one bound | Sign in with the account you used before, or ask an admin |
| Wrong domain or tenant | `forbidden` | The `hd` or `tid` required | Sign in with your example.com account |
| Token too old, or `auth_time` too old | `auth` | How old | Run `aries login` again; the provider will ask you to sign in |
| Nonce or signature wrong | `auth` | That the sign-in was not for this key | Run `aries login` again |
| The provider's keys could not be fetched | `unavailable` | The provider | Try again; the admin checks `aries admin status` |
| Device grant off | `invalid` | That the server does not offer it | Run `aries login --url` |
| Enrollment busy | `busy` | Which bound | Try again in a minute |
| Signed-in key ended (handshake) | `auth` | When it ended | Run `aries login` |
| Workload: no rule matches | `forbidden` | The provider and the required claims' names and values from the job's own token | Ask an admin to add a trust rule: `aries admin trust add` |
| Workload: rules for two principals | `forbidden` | Both rule names | Ask an admin to narrow one |
| Workload: required claim missing from a rule | `invalid` | The claim | Add `--claim NAME=VALUE` |

## 11. CLI and admin

```text
aries login [--provider NAME] [--url | --device] [--approve [CODE]]
aries login --workload KIND
aries logout
aries whoami                                    adds: how signed in, the provider, when it ends

aries admin principal add NAME [--display D] [--service --owner NAME] [--invite ADDRESS --provider P] [--key FILE] [--project P]
aries admin principal set NAME [--display D] [--owner NAME]
aries admin principal show NAME                 kind, owner, display, identities, keys with origin and end
aries admin invite add NAME ADDRESS --provider P
aries admin invite list [--pending] | remove ID
aries admin identity list NAME | remove ID
aries admin key add NAME FILE [--expires DURATION]
aries admin key list NAME                       gains ORIGIN, ENDS, MACHINE
aries admin trust add NAME --principal P --provider W --claim K=V… [--key-lifetime D]
aries admin trust list [P] | show NAME | set NAME … | disable NAME | enable NAME | remove NAME
aries admin status                              gains the providers and the counts of signed-in keys
```

CLI only, over the protocol (rule 12). Providers have no command: they are the operator's (§3.5). `aries login` and `aries logout` are a person's; `aries mcp` signs in only by opening a browser for the person (§9.4).

## 12. Protocol

Everything joins protocol 8, which no release has carried (decision 78); no bump.

- **New ops:** `enroll.begin` and `enroll.finish` (on an enrollment connection only); `principal.set`, `principal.show`; `invite.add`, `invite.list`, `invite.remove`; `identity.list`, `identity.remove`; `trust.add`, `trust.list`, `trust.show`, `trust.set`, `trust.remove`.
- **New arguments:** `principal.add`'s `display`, `kind`, `owner`, `invite`, `provider`; `key.add`'s `expires`. A server built before them refuses them as unknown fields, as rule 10 has it.
- **New result fields** (no bump needed, clients ignore what they do not know): the welcome's `key_expires`, `signed_in` (provider and flow) and `kind`; `keys`' `origin`, `expires`, `machine`; `status`' providers.
- **The bridge frame** gains `enroll` and `addr`, read leniently, as it already is between two `ariesd` of one machine.
- **Frames for the close at expiry** reuse the existing close with a code.

## 13. Threats

What each costs and what bounds it. Read with access-research.md §9, which this keeps.

| Threat | What the attacker gets | What bounds it |
|---|---|---|
| **A phished sign-in** (the attacker starts `aries login` and gets the victim to finish it) | The victim's principal on the attacker's key, for a day | Only an invited, bound identity enrolls; the URL path needs the victim to send back an address, an unusual ask; the device grant is off by default; every sign-in is announced to the principal's inbox and the audit stream with machine and provider; `principal disable` or removing the key ends it at once. A day at most otherwise |
| **A phished device code** (if the server turns the grant on) | The same | Off by default; accepted once and only within 2 minutes; the guide shows how to restrict the grant at the provider |
| **A stolen ID token** | Nothing: the nonce binds it to a key and a challenge, and the server exchanges the code itself, so the token is never on the client | Measured: the same token with another key, and again with the same key, refused |
| **A stolen code or pasted address** | Nothing without the PKCE verifier | Measured: the wrong verifier refused by the provider |
| **A stolen laptop** | The signed-in key, for the rest of the day; no refresh token | The day; `aries admin key remove` or `principal disable` at once |
| **An agent driving the person's browser** | A fresh day of sign-in with nobody present, if the provider's session is open | `prompt=login` and `auth_time` within 10 minutes (Measured to force the page in Keycloak); the browser-tab limit of `aries mcp`; announcements. Weaker for Google and GitHub, whose freshness is their session policy |
| **A misconfigured trust rule** (too broad, or the wrong repository) | Whatever the service principal may do, for whoever the rule admits | Required immutable-id claims per platform, no patterns, no bare `sub`; two matching rules refused; a member's rights at most; the rule's key lasts an hour; every `trust.*` is an event naming its author |
| **A compromised CI workflow** (a malicious pull request, a poisoned dependency) | A key for the rule's principal for an hour, from a job the rule admits | Pin `ref` or `environment` so that pull-request jobs, which carry another `ref`, do not match (that a fork's pull request gets no OIDC token at all is general knowledge, which N0's workflow checks); `job_workflow_ref` to a reviewed reusable workflow; a member's rights; decision 67's gate passing stays a person's |
| **A stolen workload token** | Nothing where bound by audience; within 2 minutes and once where not (Azure) | The binding; the window; tokens from before the daemon's start refused |
| **A compromised provider tenant, or an issuer's key** | Anyone with an identity there | Out of Aries's reach; one provider per tenant; `principal disable`; the break-glass key |
| **A wrong server clock** | A line revived at sshd if set back | The daemon refuses on the store's time; the line is gone within a second of its end; `Z` keeps the zone out of it (Measured) |
| **Strangers at the way in** | Load on the daemon | §5.4's bounds; no store opened, no slot taken |
| **The published enrollment key** | The enrollment handler, and nothing more | The bridge runs only `--enroll`; the handler's bounds |
| **The server itself compromised** | Everything in its stores, as today; codes it exchanges | Each server its own client id, so a token for one is not one for another; GitHub tokens revoked at once and with no permissions |

## 14. Plan

Each slice leaves `main` working and is off until configured. All after P1 and P2; N0 can start now, since it builds nothing. Estimates are one engineer's working weeks, from the size of comparable slices of stage 3a and the spike's code (about 960 lines for the verifier and the trust rules with their tests, and 1,400 for the flows and the stand-ins), not from a breakdown.

| Slice | Delivers | Gate | Estimate |
|---|---|---|---|
| **N0** | The real-tenant spikes (Google, Entra, GitHub App, GitHub Actions, Google Cloud, Azure) with the spike's code, by [RESULTS.md, section 7](../../spike/identity/RESULTS.md#7-the-real-tenant-spikes-what-the-maintainer-registers); both ways in on the guide's server under systemd and AppArmor (decision 54) | Each provider's row of §6.4 and §7.2 confirmed or corrected in RESULTS.md; the maintainer chooses the way in | 1 week, with the maintainer's registrations |
| **N1** | `kind`, `display_name`, `owner`; `identities`; invitations; `principal set/show`, `invite`, `identity`; the events; the display in `aries` (aries.md §17.7's `names` read) | Store and server tests for every rule of §3 and §6.5; reach and decision 19 for invitations; the soak's control-log replay knows the new events | 1.5 weeks |
| **N2** | `expires_at`, `origin`; `key add --expires`; the line with `Z`; the expiry timer; the handshake's check; closing connections at the end with both clocks; pruning; `whoami` and the notices | A server test that holds a key past its end on a fake clock and sees the line go, the handshake refuse and the connection close; the Docker smoke test with a real sshd in a zone other than UTC | 1 week |
| **N3** | The way in chosen in N0; `ariesd stdio --enroll`; `enroll.begin/finish`; the verifier and key cache; server-side code exchange; provider settings; outbound HTTPS; the bounds; the AppArmor and installer changes; `status` | Unit tests of the verifier (the spike's, moved); a fuzz target for the token parser; an e2e test against a fake issuer in process; limits' table tests | 2 weeks |
| **N4** | `aries login` (loopback, URL, `--provider`), key storage, decision 35's order with the signed-in key, `logout`, refusals; optionally `--approve` | e2e tests with the fake issuer driven by a stand-in browser, as the spike's; Keycloak in the Docker test server for a manual run | 1.5 weeks |
| **N5** | `kind: service`; `trust_rules` and `aries admin trust`; GitHub Actions, Google and Azure kinds with their required claims; audience binding; `aries login --workload`; the launch in `key.signin`; `docs/ci.md` | Table tests of each kind's required claims; e2e with a fake platform; a real GitHub Actions run in this repository's CI reporting a gate to a test server (manual) | 1.5 weeks |
| **N6** | GitHub's adapter for people; the device grant option, accepted once and fresh | Tests against a fake GitHub API; a manual run with the maintainer's GitHub App | 1 week |
| **N7** | The Claude Desktop extension (§9.6) | The manifest test; a manual sign-in from Claude Desktop on macOS and Windows | 1 week |
| **N8** | `docs/server.md` (providers, the `Match` block, limits), `docs/aries.md` (`login`), `docs/agents.md`, `docs/ci.md`, `docs/security.md`; the Docker test server with Keycloak and `smoke.sh` signing in; a security review of N1 to N7 like stage 3a's | The review's findings fixed or recorded; the smoke test passes | 1.5 weeks |

About 11 to 12 weeks in all, and N0 needs the maintainer's time for the registrations. N1 and N2 are independent of each other; N3 needs both; N4, N5 and N6 need N3 and are independent of each other; N7 needs N4.

## 15. Decisions for the maintainer

**Decided 15 Oct 2026** (design §14 of aries.md, decision 85): the maintainer accepted all twelve recommendations below as written, and kept the whole of this design in v1.0.0, with N1 to N7 worked beside the port's P4 and P5 where their files do not overlap. Asked: "Accept all twelve", and "Keep it all in v1.0.0".

Each with a recommendation.

1. **Which way in?** Decision 54 already says: spike both under AppArmor, the published enrollment key first. *Recommended:* the published enrollment key, unless N0 finds a reason against it under AppArmor or in sshd's logs. *Cost:* a known private key to explain. *Of the key command:* `AuthorizedKeysCommand` back in the block, and every stranger logged as accepted.
2. **Is the URL path the code flow with the address pasted back, on by default, with decision 55's device grant unchanged?** *Recommended:* yes (§6.3). *Cost:* a long address to copy, awkward on a phone. *Of the device grant as the URL path:* the phishing exposure decision 55 avoided, no binding, and a flow many tenants block.
3. **Does the server exchange the code, or the client?** *Recommended:* the server (§6.4). *Cost:* the server calls the token endpoint too, and holds the client secrets Google and GitHub need. *Of the client:* the ID token on the client, a client secret shipped to every client for Google, and a separate path for GitHub.
4. **`prompt=login` on every sign-in?** *Recommended:* yes where honored, with `auth_time` within 10 minutes. *Cost:* the person types a password or touches a passkey once a day per machine. *Of not:* the day renews with no one present while the browser's session lasts (Measured), which is the silent renewal decision 58 refused.
5. **The sign-in's ceiling.** Decision 58 says a day with a ceiling. *Recommended:* 24 hours by default, 7 days at most. *Cost of a lower ceiling:* none for people at desks.
6. **Build `--approve` (a new machine approved from a fresh sign-in, decision 61)?** *Recommended:* only if N0 finds a real tenant where neither the URL path nor the device grant can be used. *Cost:* a cross-device approval of Aries's own to defend.
7. **Kubernetes in v1.0.0?** *Recommended:* no; documented as configuration, untested, for 1.x. *Cost:* a team on Kubernetes keeps a stored key until then.
8. **AppArmor: the outbound rules always, or only when sign-in is on?** *Recommended:* always (§6.4). *Cost:* DNS and the certificate store readable by a daemon that may not need them.
9. **The extension: one project per entry, switched in its settings?** *Recommended:* yes (§9.2). *Cost:* several projects at once need `aries setup claude-desktop` too.
10. **The MCPB signing certificate.** *Recommended:* obtain one only once it is established what Claude Desktop does with a signed bundle; until then, the bundle is covered by the release's signed checksums. *Cost:* a certificate to buy and renew, and a secret in the `release` environment.
11. **Service principals' rights.** *Recommended:* a member's, never an admin's, until decision 8's narrower rights are designed. *Cost:* a CI job can create and update issues, as today.
12. **Where the display name lives.** *Recommended:* on `principals` (§3.1), since Postgres keeps no history. *Cost:* forgetting a person is an update, and old values stay in backups until they age out, as every row does.

## 16. Not established

- **Every real provider.** Google, Entra, Okta, GitHub, GitHub Actions, Google Cloud and Azure are from documentation; N0 runs them. In particular: whether each returns the nonce and `auth_time`, accepts a loopback redirect on any port, and honors `prompt=login` or `max_age`; whether Google's, Entra's and Okta's device grants return a nonce sent with the request, as Keycloak's did; whether a GitHub App's code exchange takes PKCE without the secret; whether a fresh Entra tenant already blocks the device grant; whether GitHub Actions and Google Cloud accept an audience of the form `AUD/n/NONCE`.
- **The way in** under AppArmor and systemd: neither candidate has run with ariesd. The published key's derivation is designed, not tried.
- **Server-side code exchange** against a provider other than Keycloak's public client; Entra's handling of a public client's code redeemed from a server is the case to watch.
- **Claude Desktop**: whether it supports MCP's URL elicitation; what it does with a signed, unsigned or self-signed bundle; whether Gatekeeper or SmartScreen inspects a binary it extracted; where it records installed extensions; whether it restarts a server when its settings change; and that an empty optional setting reaches the server as an empty variable.
- **`expiry-time`** at the scale of thousands of lines, and on OpenSSH other than 9.6p1 and 10.2p1.
- **Costs to build**: estimates, not a breakdown.
