# Transport for v1.0.0: HTTPS first, SSH kept, and sign-in on both

15 Oct 2026. **Status: accepted (decision 87); slices T1 and T2 built, 16 Oct 2026 ([T1](#as-built-16-oct-2026-slice-t1), [T2](#as-built-16-oct-2026-slice-t2)).** It records decision 86 ([aries.md](aries.md) §14), which the maintainer made on this date, and turns it into a plan; everything else it proposes waits on the questions of [§16](#16-decisions-for-the-maintainer).

**Names revised by decision 88 (15 Oct 2026):** a sign-in's lifetime is `login.token_lifetime` and `login.max_token_lifetime`, the pair `workload:` uses too ([surface-review.md](surface-review.md) §9).

**Why a document of its own.** [identity.md](identity.md) says who a principal is and how a person or a workload proves it: identities, invitations, the verifier, trust rules. This document says how `aries` reaches `ariesd` and what it carries when it does: the HTTPS listener, the pipe for Aries's frames, the tokens the server issues, the remote MCP endpoint, SSH as the second transport, the deployment profiles and the plan for the two together. identity.md keeps its tables, verifier, trust rules and provider findings, and its SSH-specific parts (expiring `authorized_keys` lines as the way a sign-in lands, the published enrollment key) are replaced here; it says so where they stand.

**Evidence.** [`spike/https`](../../spike/https/RESULTS.md) ran on 15 Oct 2026 on a Docker network marked internal, with no route out (checked from every container): Keycloak 26.8.1 on HTTPS with an internal CA's certificate, a stand-in for ariesd's listener and authorization server carrying `internal/proto`'s frames, nginx in front of it, an sshd for today's path, and the client. What it measured is marked **Measured**; what comes from a vendor's or a standard's documentation, fetched on 15 Oct 2026 and cited, is **Read**; the rest is design.

**Where it lands.** After the Postgres port's P1 and P2 (decision 79), as identity.md does: the tables are written once, in the new migration sets.

## 1. Summary

**HTTPS is the primary transport, with SSO tokens; SSH is a second transport that an operator turns on** (decision 86). A fresh server listens on HTTPS only. `aries login` signs a person in through the company's identity provider and leaves a token on the machine; `aries` and `aries mcp` then open one WebSocket to the server and speak the same NDJSON frames as today over it. The server is its own OAuth authorization server: it sends the person to the provider, exchanges the code itself (decision 85, kept), and issues a token of its own, opaque and bound to a key the client holds (DPoP, RFC 9449), for the day (decision 58). Service principals exchange their platform's token for one (RFC 8693). The same authorization server makes Aries a remote MCP server that Claude's connectors and Claude Code can add by URL.

**Air-gapped sites are first-class, with SSO.** A server with no route out runs HTTPS with a certificate from the site's own CA and an identity provider inside the network (Keycloak, AD FS), whose keys it takes from an internal URL or a file the operator supplies. Nothing in 1.0 reaches outside unless configured to ([§8.6](#86-every-place-10-reaches-outside)). SSH stays for teams that prefer keys or reach the server through IAP, and works with no network beyond the server.

**What carries over, what is replaced, what is new**, from identity.md:

| identity.md | Here |
|---|---|
| §3 Tables: `kind`, display name, owner, identities, invitations, trust rules | Carries over. Adds `grants`, `tokens` and `oauth_clients` ([§6.9](#69-tables)) |
| §6.4 The verifier, providers' quirks, server-side code exchange | Carries over, and gains an offline mode ([§8.2](#82-the-verifier-with-no-route-out)) and an Entra provider with a configured authority ([§8.5](#85-microsoft-entra-id-in-every-cloud)) |
| §6.1 to §6.3 Loopback, URL by pasting the address, device grant off | Carry over, with the redirects reversed: the provider redirects to the server, the server to the client ([§6.2](#62-a-sign-in-end-to-end)) |
| §6.7 A day, no silent renewal, `prompt=login`, `auth_time` | Carries over |
| §7 Service principals and trust rules | Carry over; the key a match got becomes a token ([§7](#7-service-principals-over-https)) |
| §4 Expiring keys as how a sign-in lands; §5 the way in before sign-in (the published enrollment key) | **Replaced** by tokens over HTTPS. `key add --expires` stays, for SSH ([§11.3](#113-expiring-keys-and-sign-in-over-ssh)) |
| §9 The Claude Desktop extension | Kept and thinner ([§10](#10-the-claude-desktop-extension)) |
| §14 Slices N0 to N8, 11 to 12 weeks | Replaced by [§15](#15-plan): about 16 weeks for transport and identity together |

**Measured, on one machine** (Apple M4 Max, containers on linux/arm64; [RESULTS.md](../../spike/https/RESULTS.md) has every figure): a request's round trip over the WebSocket was 50 µs at the median, 160 µs through nginx; opening a connection, with TLS and the token check, 0.8 ms (4.6 ms through nginx) against 80 ms for SSH to a real sshd; a pushed frame arrived in 230 µs. HTTP/2 full duplex did not survive nginx's default request buffering at all. A token copied to another key, a replayed proof, a revoked token, a revoked smart card's certificate and a person with no invitation were each refused; a connection was closed 1 ms after its token was revoked and 5 to 15 ms after its sign-in's end; and the remote MCP authorization flow ended in a tool call, all with no route to the internet.

## 2. Decision 86, and what it keeps

**The decision** (aries.md §14, decision 86), in the maintainer's words. Asked what the transport should be: "HTTPS primary, SSH kept"; the question that led to it: "If we are using SSO, shouldn't we use HTTPS as our primary means of transport?" On SSH: "We need to keep SSH for teams working in air-gapped environments", and "SSH should be disabled by default. It must be explicitly enabled during setup or installation." On priority: "I would be more concerned with SSO in an air-gapped environment." On Microsoft's clouds, teams using M365 in classified environments must be able to sign in, and "We will skip GCC High and classified testing for now." It supersedes decision 9 ("SSH stays the only transport; an HTTPS transport waits") and answers [enterprise-review.md](enterprise-review.md)'s E14 and its question "Is SSH-only a principle or a stage?": a stage.

**What stays as it is.**

- **The frames and ops** (`internal/proto`), the handshake's fields, idempotency keys, the outbox and the cache (aries.md §18). Only the pipe changes ([§5](#5-the-protocol-over-https)).
- **One authority, one writer per project** (principles 2 and 3; decision 79).
- **The store decides who is admitted**: the handshake checks the principal's standing and membership as today, whatever transport brought it.
- **Nobody adds a credential to their own account** (decisions 19 and 38); a sign-in can only bind an identity an admin invited (decision 57).
- **Rule 12.** No admin over MCP; a gate is passed only from the session `cli` (decision 68), now enforced by a token's scope ([§6.8](#68-scopes-and-the-session-cli)).
- **Rule 19.** Every time the server compares is the store's.
- **Decisions 20, 21 and 59.** Disabled wins, losing access ends claims, and a connection whose credential ended is closed.

**What changes in aries.md §2.** "No new port is opened" no longer holds: `ariesd` opens one port that other machines reach, by default. "The principal comes from which key authenticated, never from the client" holds in a new form: the principal comes from a token the server issued and checks, never from a field the client sends. What is lost is that sshd, a separate and much-reviewed program, did the authenticating and the forced command made impersonation structural; over HTTPS ariesd authenticates in its own code, on the internet. [§12](#12-security) weighs it.

## 3. Deployment profiles

The installer takes a profile, and the guide has one path per profile. Each is a set of defaults, not a different build.

| | **Internet-facing** (the default) | **Private network** | **Air-gapped** | **SSH only** |
|---|---|---|---|---|
| Who | A team on any network, hosted agents, Claude's connectors | A company network or VPN; Claude Code and `aries` inside it | A site with no route out at all | Today's servers; IAP; a site that allows only SSH |
| Transports | HTTPS | HTTPS | HTTPS, and SSH when the operator enables it | SSH (HTTPS off) |
| TLS | ACME (`autocert`), or the operator's files | The operator's files from the company's CA, or a proxy that terminates | The operator's files from the site's CA | n/a |
| Sign-in | Any provider: Google, Entra (commercial), Okta, Keycloak, GitHub | An internal or a cloud provider | An internal provider: Keycloak, AD FS, Entra in a disconnected cloud; keys from an internal URL or a file | Keys; no sign-in |
| Remote MCP connector (claude.ai, Desktop) | Yes | No: connectors connect from Anthropic's cloud ([§9.1](#91-who-uses-it)) | No | No |
| Claude Code's remote MCP | Yes | Yes | Where Claude Code itself can run | No |
| Releases | GitHub or a static host | Either | A static tree in a directory, carried in (`ARIES_RELEASES`) | Either |
| Outbound | The provider, ACME | The provider | None beyond the configured internal URLs | None |
| What the operator supplies | A DNS name; the provider's registration | A certificate, the CA's file for clients, the provider | The certificate and CA, the provider's URLs or key set file, the CRL if cards are used, a time source | The host key's fingerprint for `.aries.yaml` |

`install-server.sh --profile internet|private|airgapped|ssh-only`, with `--enable-ssh` adding SSH to any of the first three. Without `--profile` the installer asks nothing and uses `internet`; `--dry-run` prints the steps of the profile chosen.

## 4. The HTTPS listener

### 4.1 Where it sits

The listener is part of `ariesd serve`, beside the unix socket. A WebSocket on `/v1/connect` whose token checks out is handed to the same `Server.handle` that serves a socket connection today, through an in-process pipe, with the principal and the grant in place of a bridge frame ([§5.4](#54-the-handshake)). There is no `ariesd stdio` on this path, no peer check (the peer is ariesd itself), and no second process. Everything behind the handshake (dispatch, watchers, limits per principal and connection, the close when access ends) is shared by both transports.

### 4.2 TLS, three ways

```yaml
https:
  listen: ":443"                    # absent: no HTTPS (an SSH-only server)
  public_urls: [https://aries.example.com]   # what clients use; checked in DPoP proofs and used as the issuer
  tls:
    mode: acme                      # acme | files | proxy
    acme:
      domains: [aries.example.com]
      email: ops@example.com
      cache: /var/lib/aries/acme    # 0700
      directory: ""                 # empty: Let's Encrypt; or an internal ACME server's directory URL
    files:
      cert: /etc/aries/tls/cert.pem # reread on SIGHUP and when it changes
      key: /etc/aries/tls/key.pem   # 0600
    proxy:
      trusted: [10.0.0.0/8]         # who may connect; anyone else is refused
  client_ca: ""                     # a CA for client certificates (cards; 1.x, §8.4)
```

- **ACME** with `golang.org/x/crypto/acme/autocert`, already a dependency through `x/crypto`: the TLS-ALPN-01 challenge on the same port, so nothing else listens. `directory` points at an internal ACME server (step-ca, for example) where a site runs one; then ACME works with no internet too.
- **Files**: a certificate from the operator's CA, reread without a restart.
- **Proxy**: plain HTTP behind a load balancer or reverse proxy that terminates TLS and is the operator's. What must then be true, and the guide says: the proxy is the only thing that can reach the port (`trusted` is checked on the peer's address, and the installer binds to loopback or a private address); the proxy passes WebSocket upgrades and keeps an idle connection at least 60 seconds (the daemon pings every 25); `public_urls` names what clients use, since the daemon does not trust `Host` or `X-Forwarded-*` for anything that matters (**Measured**: nginx rewrites `Host` on a `location` that sets any header of its own, which broke the DPoP check until the server checked against its configured URLs). Client certificates do not pass a proxy, so smart cards need TLS at ariesd ([§8.4](#84-smart-cards)).

**TLS settings.** TLS 1.2 and 1.3 only, Go's default cipher suites (no configuration offered), HSTS on every response, no compression. A server that holds only TLS 1.3 is a setting (`min_version`), for sites whose baseline asks for it.

### 4.3 HTTP/2

On for everything but the WebSocket, which is an HTTP/1.1 upgrade: OAuth endpoints, `/mcp` and the metadata go over HTTP/2 when the client offers it. Go 1.27's server also accepts WebSocket over HTTP/2 (RFC 8441, extended CONNECT; **Read** in the standard library's source); 1.0 does not use it, since one connection per `aries` process gains nothing from multiplexing and the WebSocket code stays smaller.

### 4.4 Ports, and what stays on loopback

- **443** by default, through systemd's `AmbientCapabilities=CAP_NET_BIND_SERVICE` on the unit (the daemon stays unprivileged); `8443` where 443 is taken.
- **Stays on loopback, unchanged:** the telemetry listener (`/metrics`, `/healthz`, `/readyz`, decision 29), the status socket that `ariesd status` reads, and the unix socket the SSH bridge connects to. None is reachable over HTTPS, and the HTTPS listener serves none of their paths.

### 4.5 Paths

| Path | What |
|---|---|
| `GET /v1/connect` | The WebSocket that carries Aries's frames. Token and DPoP proof on the upgrade |
| `GET /.well-known/oauth-authorization-server` | RFC 8414 metadata of Aries's authorization server |
| `GET /.well-known/oauth-protected-resource/mcp/p/PROJECT`, and at the root | RFC 9728 metadata of the MCP endpoint |
| `GET /oauth/authorize`, `GET /auth/callback` | The sign-in: to the provider and back |
| `POST /oauth/token`, `POST /oauth/revoke` | Tokens: codes, refreshes, workload exchange; revocation (RFC 7009) |
| `POST /oauth/register` | Dynamic client registration (RFC 7591), bounded; off by setting |
| `GET /oauth/device`, `POST /oauth/device_authorization` | Aries's own device grant (RFC 8628), off by default (decision 55) |
| `POST /mcp/p/PROJECT` | The remote MCP endpoint, Streamable HTTP ([§9](#9-the-remote-mcp-endpoint)) |
| `GET /.well-known/aries` | What a client needs before signing in: the server's version, the transports on, the providers' names. No secret, no principal |

Everything else is 404. No page renders user content; the only HTML is the sign-in's few pages (consent, the device page, "you can close this tab"), with a strict `Content-Security-Policy` and `frame-ancestors 'none'`.

### 4.6 Limits (rule 18)

Each a `server.Limits` field with a default, a `limits:` key, a table-driven test and a row in docs/server.md's table.

| Limit | Default | Bounds |
|---|---|---|
| `https_conns` | 4,096 | Open TCP connections to the listener |
| `https_conns_per_addr` | 64 | Per client address (the proxy's peer address is not used; behind a proxy this limit is the proxy's) |
| `https_header_bytes` | 16 KiB | Request headers |
| `https_header_timeout` | 10 s | Reading the headers; the slow client's bound |
| `https_body` | 64 KiB | The body of an OAuth request; `/mcp` takes `proto.MaxFrame` (4 MiB) |
| `upgrade_timeout` | 10 s | From the upgrade to the hello |
| `ws_message` | 4 MiB | One message: one frame, `proto.MaxFrame` |
| `auth_failures_per_addr` | 30 a minute, burst 60 | 401s per address before `busy`; refusals counted, never logged with the token |
| `signin_pending` | 10,000 | Sign-ins started and not finished, 10 minutes each |
| `oauth_clients` | 1,000 | Registered clients (DCR); unused ones pruned after 30 days |
| `dpop_proofs` | 100,000 | Proofs remembered for their two-minute window (**Measured**: a full set refuses with `busy`) |
| `token_exchanges_per_rule` | 120 an hour | Workload exchanges per trust rule |
| `grants_per_principal` | 64 | Live sign-ins (machines and connectors); identity.md's `login_keys_per_principal` becomes this |

The existing per-principal limits (`conns_per_principal`, `handshakes_per_principal`, `writes_per_second`) apply unchanged behind the handshake.

### 4.7 AppArmor, systemd and the installer

- **AppArmor** (`deploy/apparmor`): `ariesd-serve` gains `network inet stream` listening (it has connecting, for Dolt and telemetry), `capability net_bind_service`, read of `/etc/aries/tls/**`, read and write of the ACME cache, and `abstractions/ssl_certs` (decision 85 already allows it always). `ariesd-stdio` does not change; it is not installed when SSH is off.
- **systemd**: `AmbientCapabilities=CAP_NET_BIND_SERVICE` and `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` for port 443; `RestrictAddressFamilies` already allows `AF_INET` and `AF_INET6`.
- **The installer**: the profile ([§3](#3-deployment-profiles)), the port, the TLS mode and its files, opening the port in `ufw` if it is active (and saying so), and `--enable-ssh`. `scripts/installserver_test.go` holds the steps to the guide's as today.

## 5. The protocol over HTTPS

### 5.1 Three pipes, measured

The protocol is a long-lived, ordered, two-way stream: requests and their responses one way and back, and frames the server pushes whenever an event happens (watchers, decision 27's trace context per request, close frames). Three ways to carry it over HTTPS were built in the spike, each carrying `internal/proto`'s frames unchanged, and measured directly and through nginx 1.30.6 with its defaults (request and response buffering on) and the two lines a WebSocket needs:

| Pipe | Direct: connect, round trip p50 / p99, push p50 | Through nginx | What it costs |
|---|---|---|---|
| **A WebSocket per connection** | 0.78 ms; 50 / 130 µs; 230 µs | 4.6 ms; 160 / 250 µs; 290 µs. Idle 75 s, then used: worked (the server's pings) | An HTTP/1.1 upgrade; the proxy must pass it |
| **HTTP/2 full duplex** (one POST, frames in the body and the response) | 5.0 ms (one sample); 100 / 310 µs; 270 µs | **Failed**: no response in 5 s; nginx held the request body until it ended, as `proxy_request_buffering on` does | Works only end to end over HTTP/2 or through a proxy configured for streaming (`grpc_pass`-style); corporate TLS-inspecting proxies are worse still (not measured) |
| **A POST per request, and server-sent events for pushes** | n/a; 190 / 530 µs; 200 µs | 780 µs / 2 ms; 300 µs. nginx 1.30.6 passed the events without `X-Accel-Buffering: no` | A connection's state (the session, its watchers, its limits) must live apart from any connection; a new DPoP proof per request |

SSH to a real sshd (OpenSSH 9.6p1), forced command `cat`, for comparison: connect 80 ms, round trip 90 / 170 µs.

### 5.2 The choice: a WebSocket per connection

**Recommended, and what the rest of this document assumes.** It is the same shape as the SSH channel: one ordered, two-way stream per `aries` process, so the server's session, its watchers, the close when access ends and every per-connection limit stay as they are, and the change is only the pipe. It survived the buffering proxy; HTTP/2 full duplex did not. Request/response with SSE also survived, and would suit a stateless server better, but it would move the connection's state into a session store and change how the outbox and the cache see a dropped connection; that is a redesign of the server for a pipe, the opposite of the brief. Every cloud load balancer the guide names (Google's, AWS's ALB, Azure's Application Gateway) and every common reverse proxy passes WebSockets (**Read**, general knowledge; the guide names the setting for each).

The cost: a proxy must be told to pass the upgrade, and a TLS-inspecting corporate proxy may refuse or break a WebSocket. For a client behind one, `aries` reports the failed upgrade with a fix naming the proxy, and SSH stays. A fallback pipe (request/response with SSE) is not built in 1.0; [§16](#16-decisions-for-the-maintainer), question 3.

### 5.3 The dependency question

A WebSocket needs either a library or code of Aries's own.

| | Lines | Notes |
|---|---|---|
| `github.com/coder/websocket` (ISC) | A dependency | Maintained, no transitive dependencies, compression and HTTP/2 support Aries would not use |
| `golang.org/x/net/websocket` | Already in `go.sum` (indirect) | Its own documentation points to other packages as more complete and maintained; no ping or close-code control |
| **The spike's `ws` package** | 354 with comments; 202 of tests | The opening handshake on both sides, text and binary messages, fragments, ping and pong, close, masking enforced per side, a size bound; no extensions, no subprotocols. A fuzz target ran 5.3 million inputs in 20 s with no finding |

**Recommended: no new dependency.** Move `ws` to `internal/wsconn` with its tests and fuzz target, as rule 4 asks and as the spike's `jose` was for tokens. What a library would add (compression, HTTP/2, an API for many uses) Aries does not need; what it would remove is 354 lines to review, which the security review reads anyway. `aries` links it, so `cmd/aries` grows by the client half only (`net/http` is already linked).

### 5.4 The handshake

Unchanged frames, in a new order of trust:

1. The client opens `wss://HOST/v1/connect` with `Authorization: DPoP TOKEN` and `DPoP: PROOF` (or a client certificate, [§8.4](#84-smart-cards)). The server checks the token, its grant, the proof and the binding before it upgrades; a refusal is an ordinary 401 or 403 whose body is a `proto.Error` in JSON (code, message, fix), so `aries` prints it as it prints a refused welcome today.
2. The server hands the connection to `Server.handle` with the grant's principal and the grant's id in place of the bridge frame's `pr` and `k`. As a bridge frame names a key the store must still hold, the grant must still be live; its end and its revocation close the connection, as a key's removal does.
3. Then hello and welcome exactly as today: `v`, `p` (the protocol range check, code `version`), `proj`, `s`, `m`, `h`. The project is in the hello, not the URL, as today; the welcome echoes `pr`. A hello never names a principal.

No protocol bump: nothing in a frame changes (decision 78 holds). `aries` learns which transport to use from `.aries.yaml` ([§5.7](#57-the-client)).

### 5.5 Push, watchers and pings

`watch` and the `evt` frames are unchanged. The server sends a WebSocket ping every 25 seconds on a connection with nothing to say, so that proxies with a 60-second idle timeout keep it (**Measured** through nginx: idle 75 s, then a request answered in 1.0 ms). The daemon's `idle_timeout` is unchanged and counts frames, not pings.

### 5.6 Idempotency, the outbox and the cache

Unchanged on the wire: a queued write carries its idempotency key and its base rev as today (aries.md §18.3 to §18.5). What changes is `client.Unreachable`, which decides whether a failed write is queued: over HTTPS it is true for a DNS failure, a refused or reset TCP connection, a TLS handshake that times out, and a 502, 503 or 504 from a proxy; it is false for a 401 or 403 (a refusal: `aries login`, or ask an admin), for a TLS certificate that does not verify (a configuration error, or an attack: never queue), and for a 4xx from the server. `seen.json` and the outbox are keyed by the server's identity: for SSH the chain of host keys (decision 62), for HTTPS the first of `public_urls` the welcome names. A server reached both ways is two keys to the outbox, which is correct: queued writes are replayed on the transport that queued them, by the principal that signed in on it.

### 5.7 The client

`.aries.yaml` gains `server.url`:

```yaml
project: 00000000-0000-4000-8000-000000000001
server:
  url: https://aries.example.com        # HTTPS: the default transport
  host: aries.example.com               # SSH, if the server has it on
  host_key: SHA256:…
  ca: |                                 # optional: the site's CA, for a machine whose system store lacks it
    -----BEGIN CERTIFICATE-----
```

`aries` uses `url` when it is set, and SSH otherwise; `ARIES_TRANSPORT=ssh` (or `transport: ssh` in the user's own config, never the committed file) chooses SSH where both are set, for IAP. `ca` holds a public certificate, so it may be committed; it is added to the system's roots for this server only, never in place of them. `aries admin project add` prints `url` for a server with HTTPS on, and `host` and `host_key` too when SSH is on.

## 6. Tokens

### 6.1 Aries is its own authorization server

**Recommended: ariesd issues its own tokens, after a sign-in at the provider, and accepts no provider's token at its resources.** The alternative is to delegate: accept the provider's access tokens for an API registered at the provider for Aries.

| | Aries issues its own (recommended) | Delegate to the provider's tokens |
|---|---|---|
| Google, GitHub | Work: the provider signs the person in; Aries issues | Do not work: neither issues access tokens for a third party's API |
| Entra, Okta, Keycloak, AD FS | Work | Work, with an API registration and scopes per provider |
| Remote MCP connectors | Work: Aries's metadata, PKCE, the `resource` indicator and client registration are Aries's own, so Claude registers with Aries and never with the company's provider | Each provider must support what MCP asks (RFC 8707 audiences, CIMD or DCR), and the company must let Claude register with its provider (Entra needs the MCP URL as an Application ID URI on a verified domain, **Read**) |
| Binding the token to the client's key (DPoP) | Aries decides | Only where the provider issues DPoP-bound tokens |
| Revocation, and closing connections (decision 59) | At once: the token is a row | At the token's `exp`, or by introspection at every request |
| The provider down, or unreachable after sign-in | Signed-in machines keep working for the day | Every new connection fails |
| What Aries holds | Opaque tokens, hashed; no signing key | Nothing |
| What Aries must get right | An authorization server: endpoints, codes, PKCE, refresh rotation | A verifier |

The cost is real: an authorization server is the most attacked part of any product that runs one, and the security review ([§12.4](#124-what-a-security-review-must-look-at-before-10)) is weighted to it. It is small here because it has one upstream (the configured providers), two kinds of client (`aries` and MCP connectors), no consent screens of its own beyond one page, and no token format to sign.

### 6.2 A sign-in, end to end

`aries login` by the loopback path ([identity.md §6.1](identity.md#61-flows-and-which-is-the-default) carries over; the provider never sees the client):

1. `aries` reads `/.well-known/aries` for the providers, makes a P-256 key for DPoP, a PKCE verifier and a state, and listens on `127.0.0.1`.
2. It opens the browser at the server's `/oauth/authorize` (client `aries-cli`, redirect `http://127.0.0.1:PORT/callback`, PKCE S256, `resource` the server's URL, scope `cli mcp`).
3. The server remembers the request for 10 minutes and redirects the browser to the provider with **its own** PKCE, state, nonce, `prompt=login` and `max_age` (decision 85), and its one registered redirect, `https://aries.example.com/auth/callback`.
4. The person signs in at the provider, with whatever the provider asks: a password, a passkey, a smart card ([§8.4](#84-smart-cards)).
5. The provider redirects to the server's callback. The server exchanges the code at the provider's token endpoint (decision 85's server-side exchange, carried over: now naturally, since the server is the provider's client), verifies the ID token (identity.md §6.4: issuer, audience, signature, times, nonce, `auth_time`), finds or binds the invited identity, and creates a grant for the day.
6. It redirects the browser to the client's loopback address with an Aries code, the client's state and `iss` (RFC 9207).
7. `aries` checks the state and `iss`, and exchanges the code with its PKCE verifier and a DPoP proof. The server binds the grant to the proof's key and returns one access token for the day, `token_type: DPoP`, and no refresh token.

**Measured**: the whole of it, with the stand-in browser, in 44 to 430 ms; one provider page shown (`prompt=login`); a 52-character token; no refresh token.

**What this changes at the provider**, for the better: Aries is one confidential web application with one exact redirect, not a public desktop client with loopback redirects on any port. AD FS, which matches redirects exactly (**Read**), Google's web client, and an Entra tenant whose policy refuses public clients all take that. The client secret is the server's, in a 0600 file (rule 2), as identity.md already had it for Google and GitHub.

**The URL path** (identity.md §6.3) is unchanged for the person: `aries login --url` prints the server's `/oauth/authorize` URL, the person opens it in any browser, and pastes back the loopback address the browser could not open. The code in it is Aries's, useless without the verifier.

**The device grant** becomes Aries's own: `/oauth/device_authorization` gives a code, the person opens the server's `/oauth/device` page on a phone, signs in at the provider through it, and the waiting `aries` gets its token. The provider sees an ordinary web sign-in, so a tenant that blocks the device code flow at its provider (Entra's managed policy, CISA's baseline) does not block this. The phishing that decision 55 guarded against is Aries's to guard against now: off by default, the page shows the machine's name and address, a code is good for 10 minutes and once, and the sign-in is announced to the principal's inbox.

### 6.3 The token

An Aries access token is 256 random bits, base32 (52 characters), stored only as its SHA-256 in the control store. It carries nothing; the server looks it up. **Recommended over a signed JWT** because the server is the only party that ever checks it, revocation is a delete that takes effect at once, and Aries holds no signing key to protect, rotate or leak (identity.md's property, kept). With several daemons ([§13](#13-horizontal-scaling)) every daemon reads the same table. The lookup is one indexed read, cached for a minute with the grant's end and revocation pushed to the cache.

### 6.4 Binding the token to the machine

| | DPoP (RFC 9449), recommended for `aries` | mTLS-bound (RFC 8705) | Bearer |
|---|---|---|---|
| A token copied off the machine alone (a log, a proxy, a shoulder) | Useless without the key | Useless without the certificate's key | Works anywhere until it ends |
| Through a proxy that terminates TLS | Works: the proof is a header | Breaks: the certificate ends at the proxy | Works |
| What the client holds | A P-256 key in a 0600 file | A key and a certificate the server issued | Nothing more |
| Cost per request | One ES256 signature; one verification and a remembered `jti` at the server | None after the handshake | None |
| Who supports it | `aries` (Aries's own client); not Claude's connectors (their documentation names no DPoP, **Read**) | Few clients | Everyone |

**Recommended:** DPoP for every token `aries` holds (people's and workloads'), and bearer for remote MCP connectors, which offer nothing else, with a shorter life ([§6.5](#65-lifetimes-and-renewal)). **Measured:** the token with no proof, with a proof from another key, and with a proof used once already were each refused; a proof replayed at the token endpoint was refused. On the WebSocket the proof is checked once, on the upgrade, so it costs nothing per request; for `/mcp` and `/oauth/token` it is per request.

What DPoP does not do: an attacker who has the user's files has the key too. A key in the operating system's keystore that cannot be exported would close that, and is 1.x ([§6.6](#66-where-the-token-lives)).

### 6.5 Lifetimes and renewal

Decision 58 holds: a sign-in lasts `login.token_lifetime` (24 hours), at most `login.max_token_lifetime` (7 days), and nothing renews it.

| Client | Access token | Refresh token | At the day's end |
|---|---|---|---|
| `aries`, `aries mcp`, the Desktop extension | The whole day, DPoP-bound | None (**Measured**: none issued) | The connection is closed (decision 59); the next command says `aries login` |
| A remote MCP connector (Claude's) | 1 hour, bearer | Rotating, each use returning a new one; never past the grant's end; a reused one ends the grant | The refresh fails with `invalid_grant`; Claude asks the person to connect again (Claude refreshes on a 401 and up to five minutes before expiry, **Read**) |
| A workload | The rule's `token_lifetime`, 1 hour by default, at most 6 | None | The job's run is over |

The connector's refresh token is not the silent renewal decision 58 refused: it renews the hour inside the day, never the day. `prompt=login` and the `auth_time` check (identity.md §6.7) apply to every sign-in, a connector's included.

### 6.6 Where the token lives

In the user's data directory (`internal/offline`'s, or `ARIES_DATA_DIR`), under `signin/SERVER/`, where SERVER is the hex of SHA-256 of the server's first public URL: `token.json` (the token, its end, the scopes, the principal) and `dpop.key` (PKCS#8), both 0600 in a directory `internal/offline` checks is the user's own, as identity.md §6.8 had it for the key. Every `aries` and `aries mcp` of the user uses it, and `aries logout` revokes the token at the server and deletes both.

**The operating system's keystore** (macOS Keychain, Windows DPAPI or CNG, a TPM): not in 1.0. Keychain needs cgo or the `security` command, and rule 4's `CGO_ENABLED=0` rules out the first; DPAPI is reachable through `syscall` and is the cheapest step in 1.x; a non-exportable key (Secure Enclave, TPM) is what would make DPoP hold against a stolen disk, and is the step worth taking after. Until then the file is what `aries` already trusts for `ARIES_KEY_FILE`, and its worth is bounded by a day.

### 6.7 Revocation, and connections when a token ends

| Event | What the daemon does | Measured |
|---|---|---|
| `aries logout`, `POST /oauth/revoke` | Deletes the grant's tokens, closes its connections with a close frame (`auth`, "your sign-in ended", fix `aries login`) | Closed 1 ms after the revocation |
| The grant's end | A timer at the end, and a check on each new connection, close it the same way | Closed 5 to 15 ms after the end, in four runs |
| `principal disable`, membership removed (decisions 20, 21) | As today, for every grant of the principal | (Today's code) |
| An admin ends one machine's sign-in | `aries admin signin list NAME`, `aries admin signin end ID` | Not built |
| A refresh token reused | Ends the grant (theft detection, OAuth 2.1) | Not measured |

### 6.8 Scopes, and the session `cli`

Every token carries scopes, and the server checks them at dispatch, by op:

| Scope | Given to | Allows |
|---|---|---|
| `cli` | `aries login` in a terminal | Every op the principal's role allows, with the session `cli`: admin ops, gate passing (decision 68), locks (decision 70) |
| `mcp` | `aries login` too, and remote connectors alone | The ops behind the MCP tools; never an admin op, never passing a gate, never the session `cli` |
| `ci` | Workload exchanges | A member's ops (identity.md §7.1) and reporting a `ci` gate's check, from the session `ci` (decision 67) |

**Measured**: a `ci` token's `gate.pass` was refused with code `forbidden` and a fix to ask a person; a `cli` token's passed. How the server knows `cli` over HTTPS is therefore the scope: a connector's token cannot pass a gate whatever the client sends. What it cannot know is whether `aries` with a `cli` token was typed by a person or run by an agent with a shell on the same machine, since both read the same file; that is today's limit over SSH too, where `aries` with the person's key can claim the session `cli`. 1.x can narrow it: `aries mcp` takes a token of scope `mcp` only, by token exchange from the `cli` one at start, so that a harness holding only `aries mcp`'s process never holds `cli`.

### 6.9 Tables

In the control schema, written through `Control.write` with an event each (rules 5 and 7), beside identity.md §3's:

| Table | Columns |
|---|---|
| `grants` | `id`, `principal`, `kind` (`person`, `service`), `client_id`, `scopes`, `jkt` (DPoP thumbprint, nullable), `resource`, `flow` (`loopback`, `url`, `device`, `workload`, `card`), `identity` or `trust_rule`, `machine`, `created_at`, `ends_at`, `revoked_at` |
| `tokens` | `hash` (primary key), `grant`, `kind` (`access`, `refresh`), `expires_at`, `used_at` (refresh tokens) |
| `oauth_clients` | `client_id`, `kind` (`builtin`, `dcr`, `cimd`, `static`), `name`, `redirect_uris`, `created_at`, `last_used_at` |

Events: `signin.grant` (principal, provider or rule, flow, machine, client, end; never a token), `signin.end` (reason: `logout`, `revoked`, `expired`, `reuse`, `disabled`). Deleting expired tokens and grants past `login.prune_after` is pruning, quiet as rule 7 allows. Sign-ins in progress, `jti`s seen and device codes are the daemon's memory, bounded ([§4.6](#46-limits-rule-18)).

## 7. Service principals over HTTPS

identity.md §7 carries over: a principal of `kind: service` with an owner; trust rules matched on immutable claims; a member's rights. What changes is what a match yields: a token, by **OAuth 2.0 Token Exchange (RFC 8693)** at `/oauth/token`, not an expiring key.

```text
POST /oauth/token
DPoP: <proof by the job's own new key>
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the platform's OIDC token>
subject_token_type=urn:ietf:params:oauth:token-type:jwt
audience=https://aries.example.com
```

The server verifies the platform's token with identity.md §6.4's verifier (the workload provider's issuer, the server's URL as audience), matches exactly one trust rule, and issues a DPoP-bound token of scope `ci` for the rule's lifetime. `aries login --workload github-actions` (or `google`, `azure`) does it and stores the token in `$RUNNER_TEMP`, as identity.md §7.4 had for the key. **Measured** with a Keycloak client-credentials token shaped like GitHub Actions': exchanged, the job connected as `ci-example`, its `gate.pass` was refused, and the same exchange's proof sent again was refused.

**Replay of the platform's token.** A platform token copied from a job could be exchanged by someone else while it lasts (5 to 60 minutes by platform). Bounds: the platform's `jti` remembered and spent once where the token has one (GitHub's does); `iat` within 2 minutes (`workload.fresh`); and the audience nonce of identity.md §7.3 (`AUD/n/NONCE`, the nonce a hash of the job's DPoP key) where the platform lets the job choose the audience, which binds the exchange to the job's key. The design keeps all three; N0 checks which each platform allows.

**`docs/ci.md`** loses its stored key for HTTPS servers: no `ARIES_CI_PRIVATE_KEY`, no secret anywhere. A stored key with `--expires` stays documented for SSH-only servers and CI systems with no workload identity.

## 8. Identity providers, and SSO with no route out

### 8.1 Provider kinds

| `kind` | For | Notes |
|---|---|---|
| `oidc` | Keycloak, AD FS, Okta, Google Workspace, any OpenID Connect provider | identity.md §6.4's verifier; `issuer` exact |
| `entra` | Microsoft Entra ID, in any cloud | `authority`, `tenant` and `token_version` required, no default authority ([§8.5](#85-microsoft-entra-id-in-every-cloud)); subject `oid`; `tid` checked |
| `github` | GitHub, internet only | identity.md §6.6's adapter |

### 8.2 The verifier with no route out

Each provider names where its keys and endpoints come from, and the verifier fetches nothing else:

```yaml
login:
  providers:
    - name: corp
      kind: oidc
      issuer: https://sso.corp.example/realms/example
      client_id: aries
      client_secret_file: /etc/aries/corp-secret
      ca_file: /etc/aries/corp-ca.pem      # the internal CA that signed the provider's certificate
      discovery: fetch                     # fetch: from issuer/.well-known, on the internal network
    - name: corp-offline
      kind: oidc
      issuer: https://sso.corp.example/realms/example
      client_id: aries
      client_secret_file: /etc/aries/corp-secret
      ca_file: /etc/aries/corp-ca.pem
      discovery: static                    # nothing is discovered: every endpoint is written here
      authorization_endpoint: https://sso.corp.example/realms/example/protocol/openid-connect/auth
      token_endpoint: https://sso.corp.example/realms/example/protocol/openid-connect/token
      jwks_file: /etc/aries/corp-jwks.json # the provider's key set, exported by the operator
```

- **`discovery: fetch`** reads the discovery document and the key set from the issuer's own URLs, which on an air-gapped site are internal, through `ca_file` (and never the system's roots in addition, unless `ca_file` is empty). It follows no redirect, and refetches keys at most once a minute for an unknown key id (identity.md §6.4).
- **`discovery: static` with `jwks_file`** reads keys from a file and fetches nothing to check a token. **Measured**: a server in this mode signed alice in against Keycloak and checked her token with no request to Keycloak's key set; the unit test `TestStaticKeysFetchNothing` (spike/identity) refuses a token of a key not in the file with a message naming the file, and counts zero fetches. The code exchange still calls the token endpoint, which is inside the network.
- **Rotation.** With `fetch`, a new key id is fetched when first seen. With a file, the operator replaces the file before the provider starts signing with a new key (Keycloak and AD FS publish the next key ahead; the guide gives each's step), and ariesd rereads it on SIGHUP; a token with an unknown key id is refused with `unavailable` and a fix naming the file.
- **`aries admin status`** shows each provider's source (fetch or file), its key ids and, for a file, when it was last read.

### 8.3 Providers inside the network

**Keycloak** is the reference, and what N0 and the Docker test server run: **Measured** end to end on HTTPS with an internal CA, offline.

**AD FS** (Windows Server 2016 and later; **Read** in Microsoft's AD FS OpenID Connect documentation, fetched 15 Oct 2026):

| What Aries needs | AD FS |
|---|---|
| Authorization code flow, confidential client | Yes (a "server application" or "web API" with a secret or a key-signed assertion) |
| PKCE | Yes, from Windows Server 2019 ("as of Windows Server 2019, AD FS supports PKCE"), S256 and plain |
| `nonce` returned in the ID token | Documented as required for ID token requests of the implicit flow; for the code flow it is not stated. N0 |
| `prompt=login` | Yes: "forces the user to enter their credentials on that request" |
| `max_age`, `auth_time` | Not stated. N0 |
| Redirect URI | Must match a registered one exactly, which the server's single callback does |
| Device code | Has a `/devicecode` endpoint; not needed, since Aries runs its own ([§6.2](#62-a-sign-in-end-to-end)) |
| Refresh tokens | Not requested (`offline_access` never asked) |
| Smart cards | Certificate authentication at AD FS, the page the browser shows; Aries sees only the token |

Microsoft's AD FS pages each say it "highly recommends migrating to Microsoft Entra ID"; AD FS remains what many disconnected sites run.

**Active Directory without OpenID Connect** (Kerberos and LDAP only). **Recommended: 1.0 requires an OpenID Connect front, and the guide shows Keycloak federating to AD** (Keycloak's user federation over LDAP, with its Kerberos authenticator for desktop single sign-on), or AD FS where the site has it.

| | An OIDC front (recommended) | LDAP bind in ariesd | Kerberos (SPNEGO) on the HTTPS listener |
|---|---|---|---|
| What ariesd handles | Tokens only | People's passwords, in ariesd's memory and over its connections | Kerberos tickets; a keytab for an `HTTP/` principal on the server |
| Second factor, smart cards | The front's | None, unless Aries builds them | Whatever got the person their ticket |
| Code in Aries | None | An LDAP client (a dependency, or a few hundred lines of BER), group lookups, lockout policy | A Kerberos library (`gokrb5`, a large dependency), SPNEGO in `aries` on three systems, and a browser-less client story |
| Works for remote MCP | Yes | No: a connector cannot send a password to Aries | No |
| Risk | One more service for the site to run | Aries becomes a password-handling service, which every review flags | The largest code and the hardest to test without a domain |

**Smart cards** are [§8.4](#84-smart-cards)'s. **Other on-premises providers** (Ping Federate, Shibboleth with an OIDC plugin, Gluu, an internal Okta) are `kind: oidc` and settings; N0 does not run them.

### 8.4 Smart cards

PIV and CAC cards are common where networks are air-gapped. Three places a card can act:

| Where | How | Aries's code | Recommended for 1.0 |
|---|---|---|---|
| **At the provider** | AD FS certificate authentication, Keycloak's X.509 browser authenticator, Entra's certificate-based authentication (in commercial and Government clouds, **Read**). The browser asks for the card's PIN; the provider checks the certificate and its revocation | None: Aries receives an ID token like any other. An optional `require_amr` per provider could insist the token says a certificate was used (Entra's `amr` includes `rsa`; AD FS's and Keycloak's claims vary); N0 | **Yes.** The guide's way to use cards |
| **At ariesd, by mutual TLS** | The client presents the card's certificate in the TLS handshake (`client_ca` set); ariesd checks the chain to the site's CA and revocation (a CRL file the operator refreshes, or OCSP to an internal responder), and maps the certificate to a principal | A `certificates` table (subject or SAN to principal), CRL and OCSP checks, `aries` reaching the card through PKCS#11 or the OS store (cgo on macOS) | **No; 1.x.** **Measured** in the spike with software certificates standing in for cards: alice's admitted, mallory's refused by the CRL. But it needs TLS at ariesd (not at a proxy), a revocation feed, and a cgo-free way to reach the card from `aries`, which does not exist |
| **Over SSH** | The card's key through ssh-agent (OpenSSH's PKCS#11 support, or the OS's agent), added to Aries by an admin as an ordinary key; or SSH certificates from the site's CA (aries.md §17, option B) | None for keys: `aries` already offers ssh-agent's keys (decision 35) | **Keys: yes, today**, and the guide says how. **Certificates: 1.x**, as §17 has them |

### 8.5 Microsoft Entra ID in every cloud

**Read**, in Microsoft's documentation fetched 15 Oct 2026: "National clouds are physically isolated instances of Azure", each "with its own environment and endpoints"; an application must be registered separately in each.

| Cloud | Who | Authority host | Notes |
|---|---|---|---|
| Commercial, and Microsoft 365 GCC | Most tenants; GCC "was designed to use Microsoft Entra Public" | `https://login.microsoftonline.com` | Issuer v2.0: `https://login.microsoftonline.com/{tid}/v2.0`; v1.0: the tenant's v1 discovery document's (`https://sts.windows.net/{tid}/`) |
| Azure Government: Microsoft 365 GCC High and DoD | Tenants in Microsoft Entra Government (`tenant_region_scope` `USG` or `USGov`) | `https://login.microsoftonline.us` | Issuer: the tenant's discovery document's; v2.0 is expected to follow the commercial form under this host (inferred, not read) |
| Microsoft Azure operated by 21Vianet | China | `https://login.partner.microsoftonline.cn` | Listed for completeness |
| Azure Government Secret, Azure Government Top Secret | Classified; "dedicated US air-gapped regions" | Exists only inside each cloud | Microsoft's public comparison excludes them ("contact your account team"); nothing about them can be read from outside |

- **Token versions.** A resource's tokens are v1.0 or v2.0 by its registration's `requestedAccessTokenVersion`, and either endpoint can issue either; ID tokens follow the endpoint. Aries asks the v2.0 endpoint, and `token_version` must match what the registration issues; the verifier checks `ver`.
- **Configured, never assumed.** The `entra` provider takes `authority`, `tenant` and `token_version` and has no default authority; `token_version: 1` requires `issuer` written out, copied from the tenant's own v1.0 discovery document. The verifier pins the issuer exactly and checks `tid` against `tenant`; keys come from the authority's key set through the configured `ca_file`, or from `jwks_file`. `common` and `organizations` are refused. **Tested** (`spike/https/provider`): a stand-in issuer serving Entra-shaped v2.0 tokens at an authority that is not the commercial cloud's, with `tenant_region_scope` `USGov`, verified through its own CA; a token naming the commercial issuer, another tenant, version 1.0 or no `oid` was refused; with a key set file and the authority shut down, nothing was fetched; a provider with no authority, no tenant or no version was refused at start; and a test fails if `provider.go` names any provider's host.
- **Managed identities and workload identity federation** in Azure Government: Entra's feature list for Government shows "Microsoft Entra Workload Identities Premium edition is available in Azure for US government" (**Read**); whether IMDS managed identities and federated credentials behave there as in commercial Azure, and what their tokens' issuer is, was not established from the pages read. In Secret and Top Secret: unknown from outside.
- **Policies these tenants commonly enforce.** Device code blocked by Conditional Access: Aries's default (the code flow with PKCE, through the server) is not the device code flow, and Aries's own device grant is a web sign-in at the provider ([§6.2](#62-a-sign-in-end-to-end)). Certificate-based (smart card) authentication: done inside Entra; Aries only receives the token. Conditional Access on device compliance: applies to the browser's sign-in, which Aries does not see.
- **Not tested: deferred by the maintainer.** No GCC High, DoD, Secret or Top Secret tenant is in N0 or the plan ("We will skip GCC High and classified testing for now"). 1.0 claims for those clouds "configurable, not tested", and the guide says so.

### 8.6 Every place 1.0 reaches outside

| What | Where it goes | Setting | Air-gapped |
|---|---|---|---|
| Sign-in: discovery, keys, code exchange | Each provider's URLs | `login.providers[*]` | Internal URLs, or `jwks_file` and the endpoints written out |
| Workload token checks | Each platform's issuer and keys | `workload.providers[*]` | GitHub Enterprise Server's or the cluster's own issuer, or a key set file; or none |
| GitHub's adapter | `github.com`, `api.github.com` | `kind: github` | Absent |
| ACME | Let's Encrypt, or `acme.directory` | `https.tls.mode: acme` | `files`, or an internal ACME server |
| CIMD: fetching a connector's client metadata | The client id's URL (`claude.ai` for Claude) | `mcp.cimd_hosts` | Off |
| Releases: `aries upgrade`, `ariesd upgrade`, `install.sh` | GitHub, or a static host | `ARIES_RELEASES`, `releases:` | A static tree in a directory |
| Telemetry export | The operator's collector | `OTEL_*`, off by default | An internal collector, or off |
| The release check in the welcome | Nothing: `latest:` is set by hand (aries.md §11) | `latest:` | Unchanged |
| OCSP for client certificates (1.x) | The responder in the certificate | `client_ca`, `ocsp` | An internal responder, or the CRL file |
| `aries` itself | The server; the IAP tunnel's `gcloud` where configured | `.aries.yaml` | Unchanged |

`ariesd check` (decision 64's `ariesd doctor`) lists what this server's settings would reach, and with `--offline` fails if any of it is outside the networks the operator names.

### 8.7 Time

Rule 19 holds: every comparison (a token's `exp`, `iat`, `auth_time`, a grant's end, a DPoP proof's `iat`) is on the store's time. A site with no route out takes its time from the network's own source (chrony or `systemd-timesyncd` to an internal NTP server, or a GPS or PTP clock); the guide's "The server's clock" section gains that path. A provider and Aries that disagree by more than `login.skew` (30 s) refuse sign-ins with a message naming the difference, which is the usual first symptom of a site without time.

## 9. The remote MCP endpoint

### 9.1 Who uses it

**Read** in Anthropic's documentation, fetched 15 Oct 2026 ([custom connectors](https://support.claude.com/en/articles/11175166-getting-started-with-custom-connectors-using-remote-mcp); [authentication for connectors](https://claude.com/docs/connectors/building/authentication); [Claude Code's MCP page](https://code.claude.com/docs/en/mcp)):

| Client | How it reaches Aries | OAuth | Fits |
|---|---|---|---|
| **claude.ai, Claude Desktop, Claude mobile, Cowork** (a custom connector) | **From Anthropic's cloud, not the user's machine**, "across every Claude client"; the server "must be reachable over the public internet from Anthropic's IP ranges" (`160.79.104.0/21`) | Claude's published Client ID Metadata Document when the authorization server advertises `client_id_metadata_document_supported: true` and `none` among its token endpoint auth methods, else dynamic registration, else a client the user enters; redirect `https://claude.ai/api/mcp/auth_callback`; PKCE S256 always; only the first of `authorization_servers` is used; a 401 is required to start; 10 s for discovery, registration and token endpoints, 30 s for refresh | Internet-facing servers. Team and Enterprise plans: an Owner adds the connector, each member connects |
| **Claude Code** | From the user's machine (`claude mcp add --transport http NAME URL`) | Its own CIMD, else DCR, else `--client-id`; a loopback redirect on any port, as `localhost` and `127.0.0.1`; refuses a non-HTTPS token endpoint except on loopback; validates `iss` | Every profile where Claude Code runs, private networks included |
| **`aries mcp`** (stdio, and the Desktop extension) | `aries`'s own WebSocket | Not MCP's: `aries login` | Every profile |

Since Aries is its own authorization server ([§6.1](#61-aries-is-its-own-authorization-server)), only Aries must be reachable from Anthropic's range, never the company's provider: the person's browser reaches the provider, and Aries's callback reaches it from the server. That is the authorization design's largest gain for connectors.

### 9.2 The protocol revision

MCP's current revision is **2026-07-28** (**Read** at commit `3f6e5e1` of `modelcontextprotocol/modelcontextprotocol`, 22 Sep 2026). It removed protocol-level sessions and the `Mcp-Session-Id` header, removed `initialize` (each request carries its protocol version and capabilities in `_meta`), replaced the GET stream with `subscriptions/listen`, removed stream resumability, and deprecated dynamic client registration in favor of Client ID Metadata Documents. `github.com/modelcontextprotocol/go-sdk` v1.8.0, already Aries's dependency, speaks 2026-07-28 and the four revisions before it, and serves it statelessly (**Measured**: the spike's endpoint answered the SDK's client, and a hand-made request without `_meta`'s client capabilities was refused by the SDK with `-32602`). Claude's clients' revision is not stated in the pages read.

### 9.3 Authorization

What the specification's [authorization section](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/3f6e5e17e2a83b47dfb37b20d159e0e27efa316c/docs/specification/2026-07-28/basic/authorization/index.mdx) requires, and what Aries does:

| Requirement | Aries |
|---|---|
| Protected resource metadata (RFC 9728), at the well-known path or named in the 401's `WWW-Authenticate` | Both; `resource` equals the connector's URL exactly; one authorization server, Aries (**Measured**: the 401 named it) |
| Authorization server metadata (RFC 8414 or OIDC discovery) with `code_challenge_methods_supported` | RFC 8414, S256 only |
| PKCE S256; `resource` (RFC 8707) in both requests; `iss` in the response (RFC 9207) | All three; the token's audience is the resource |
| Client registration: CIMD (should), pre-registration, DCR (may, deprecated) | **CIMD**, with a host allowlist (`mcp.cimd_hosts`, `claude.ai` by default on the internet profile) and SSRF-safe fetching (public addresses only, no redirects, 5 s, 16 KiB); **DCR** as the fallback, bounded by `oauth_clients` and prunable; **static clients** in the settings for air-gapped and private sites. **Measured**: DCR then a tool call |
| Tokens only for this resource; no passthrough | The token is Aries's, for `/mcp/p/PROJECT` only (**Measured**: refused at `/v1/connect`, and the CLI's token refused at `/mcp`) |
| `Origin` validated on every request (DNS rebinding) | Refused unless it is the server's own or a configured client's (**Measured**: `https://evil.example.com` 403; `https://claude.ai` passed) |
| Short-lived tokens; rotating refresh for public clients | 1 hour; rotating, ending with the sign-in ([§6.5](#65-lifetimes-and-renewal)) |
| A consent page that shows the redirect's host | One page, Aries's own, after the provider: the client's name, the redirect's host, the project, the scopes |

**Enterprise Managed Auth** (Claude exchanging an identity assertion from the company's provider by the JWT bearer grant, RFC 7523, so that members connect without a consent step, **Read**) is 1.x: it makes Aries trust the provider's assertions issued to Claude, a different trust than the sign-in's, and needs a Claude organization to test.

### 9.4 Sessions

MCP 2026-07-28 has no session, and a connector has no process that runs between tool calls. Aries's session for a connector is derived: one per grant (a person's connection of one client) and client, named `mcp-` and eight hex digits, stable across the conversation's requests (**Measured**: two stateless calls gave the same). It shows in `who`, is recorded on events as any session is, and starts `prime` the first time it is seen.

**Claims.** Principle 5 has the harness renew leases; a connector has no harness. A claim taken through a connector is renewed by each tool call of that session, and lapses `lease` after the last, as a dead harness's does. For a chat that plans and files issues, that is right; for long work it is not, and the guide says so: long work belongs in Claude Code or an agent with `aries mcp`.

**The project** is in the URL: `https://aries.example.com/mcp/p/PROJECT`, so one connector is one project (identity.md §9.2's reasoning, decision 85's question 9) and the resource, and so the token, are per project. A person adds one connector per project.

### 9.5 Rule 12

The endpoint serves `internal/mcpserver`'s tools, the same list as `aries mcp`, in process: the server runs the MCP server against an in-process client connection with the token's grant, so the tools, budgets and rewording are one code. A connector's token has scope `mcp` only, so admin ops and gate passing are refused at dispatch, not only absent from the tool list ([§6.8](#68-scopes-and-the-session-cli)). The tool-schema budget (decision 76) is unchanged.

This puts `internal/mcpserver` and the MCP SDK into `ariesd`, which today links neither. Rule 4: the SDK is already a dependency of `aries`; `ariesd`'s binary grows (to be measured in the slice), and `TestInitAllocations` must still hold the bridge's start, which means the bridge's build must not initialize the MCP server.

### 9.6 What the maintainer can try with Claude Desktop

A custom connector connects from Anthropic's cloud, so the spike on a laptop cannot be added as it stands: it needs a public HTTPS address with a certificate a public CA issued, and Keycloak reachable by the maintainer's browser. [RESULTS.md, section 8](../../spike/https/RESULTS.md#8-trying-the-spike-as-a-connector-in-claude-desktop) has the steps and what to observe.

## 10. The Claude Desktop extension

**Recommended: keep it, thinner.** Remote connectors cover Desktop users on internet-facing servers with no binary at all, and the guide leads with them there. The extension (identity.md §9) stays for what a connector cannot do:

- **Private and air-gapped networks**: a connector connects from Anthropic's cloud and cannot reach them; the extension's `aries mcp` runs on the person's machine.
- **Work that needs the machine**: git paths, the outbox and the cache, a harness that renews leases.
- **Organizations that do not allow custom connectors**, or whose plan limits them (Free: one).

What changes: no SSH and no enrollment key. The extension's `aries mcp` signs in by [§6.2](#62-a-sign-in-end-to-end)'s loopback flow, opening the browser (identity.md §9.4's tab limit and URL elicitation stay), and keeps the token where `aries login` does. Its settings lose the host key and the key file and gain `url`; `ca` stays optional. N7 shrinks to about half a week.

## 11. SSH as the second transport

### 11.1 Off by default

A fresh server serves HTTPS only. SSH is on when the operator says so:

```yaml
transports: [https, ssh]   # default [https]; [ssh] alone serves a server with HTTPS off
```

| | SSH off (the default) | SSH on |
|---|---|---|
| The installer | Writes no sshd drop-in, no `aries` account's `.ssh`, no forced command, no `ariesd-stdio` AppArmor profile | `--enable-ssh` (or `--profile ssh-only`): writes them, as today's guide does, and checks `sshd -t` |
| ariesd | Writes no keys file, and removes one it wrote before; refuses a bridge frame with code `unavailable`, "SSH is off on this server", fix "an operator sets transports: [https, ssh] in ariesd's config and runs install-server.sh --enable-ssh", which `ariesd stdio` relays to the client as it relays every refusal | Writes the keys file; serves the bridge as today |
| `aries admin key …` | **Allowed**, with a notice line: "SSH is off on this server; this key takes effect when an operator turns it on". Recommended over refusing, so an operator can stage keys before turning SSH on, and because keys are also how service principals with no workload identity are named | As today |
| `aries admin status`, `ariesd status` | `transports: https (port 443, certificate ends 2027-01-13)`, `ssh off` | `ssh on (N keys in the file, M bridge connections)` |

**Turning SSH on** is two steps the guide gives in order: `install-server.sh --enable-ssh` (which checks before it acts, as every step does), then the setting and `systemctl reload ariesd`. The daemon checks at reload that the bridge's socket directory and the sshd drop-in exist and says which is missing if not; then it writes the keys file. **Turning it off**: the setting, `reload`; the daemon closes every bridge connection with a close frame (`unavailable`, "SSH was turned off on this server", fix to use the server's URL), empties and removes the keys file, and refuses bridges from then; `install-server.sh --disable-ssh` removes the drop-in.

**HTTPS off** (`transports: [ssh]`) is allowed, at least one transport must be on, and sign-in then is refused at start (`login:` needs HTTPS) with a fix naming both settings. **Recommended**: allow it, since it is exactly today's server and serves IAP-only and SSH-only sites.

**The client.** `aries` configured for SSH against a server with SSH off is refused by sshd before Aries can say anything (the `aries` account has no keys), so `aries` turns an authentication failure into a refusal with code `auth` and the fix "the server may have SSH off: if `.aries.yaml` has `server.url`, use it (unset `ARIES_TRANSPORT`); otherwise ask the server's operator". When sshd still runs the forced command (between the two steps above), the daemon's refusal is precise.

**Tests and the gate.** `scripts/installserver_test.go`: the default profile's dry run writes no sshd drop-in, no `authorized_keys`, no `aries_keys`, no `ariesd-stdio` profile, and opens no port 22 rule. `internal/server`: a daemon with default settings refuses a bridge frame with code `unavailable` and writes no keys file; turning SSH off closes bridge connections and removes the file; key admin ops with SSH off succeed with the notice. `cmd/ariesd`: the default config's `transports` is `[https]`. The Docker test server's smoke test runs both: HTTPS only, then `--enable-ssh`.

### 11.2 What SSH serves, with no network

Everything in 1.0 that does not depend on sign-in works over SSH on a server with no route out: issues, claims, gates and locks (decisions 66 to 70), memory, the outbox and the cache, `aries mcp` and the Desktop extension (with a key file), releases from a static tree, service principals by a stored key or a key with `--expires`. What needs HTTPS, and how each fails over SSH:

| Feature | Over SSH | Refusal |
|---|---|---|
| `aries login` (people) | Not available | `invalid`: "sign-in needs the server's HTTPS address", fix "set server.url in .aries.yaml, or ask your admin for a key" |
| `aries login --workload` | Not available | The same, fix "use a stored key: docs/ci.md" |
| The remote MCP endpoint | Not available | n/a (HTTPS only) |
| Anything needing the internet (GitHub's adapter, ACME, a hosted provider) | n/a | The server's settings refuse at start, naming the profile |

### 11.3 Expiring keys and sign-in over SSH

identity.md §4 and §5 made sign-in land as an expiring line in sshd's file, reached through a published enrollment key. With HTTPS carrying sign-in, **recommended for 1.0**:

- **Keep `aries admin key add --expires`** with its `expiry-time` written in UTC with `Z` (identity.md §4.1, Measured), the handshake's check and the close at the end (§4.2 to §4.4): about three days of the old N2, and what an admin needs for a contractor's key or a stopgap CI key on an SSH-only server.
- **Drop the published enrollment key and SSH sign-in from 1.0.** The way in before sign-in is HTTPS now; the enrollment path existed only because SSH was the only transport.
- **SSO-minted SSH keys for air-gapped teams: 1.x, if asked.** An air-gapped team with an internal provider gets SSO over HTTPS in 1.0 with no SSH at all. A team that must use SSH with SSO would get `aries login --ssh`: sign in over HTTPS, and the server records an expiring key for the grant's day (identity.md §4's line, measured). It reuses the grant and the expiring keys above, so it is about a week when someone needs it; it needs HTTPS reachable from the client for the sign-in, which a team on IAP alone does not have.

## 12. Security

### 12.1 From a forced command to a token

| | SSH today | HTTPS |
|---|---|---|
| Who authenticates | sshd, by the key's line | ariesd's own code: TLS, the token lookup, the DPoP proof, the scope |
| How the principal is bound | Structurally: the forced command names it | By the grant behind a token the server issued; never a field the client sends |
| Exposure | sshd on 22, the most reviewed network daemon there is | A Go HTTP server, an authorization server and an MCP endpoint on 443 |
| Credentials on the client | A long-lived key, or none (agent) | A day's token and a key that binds it |
| Revocation | Removing the key's line; the daemon closes connections | Deleting the grant; at once |

The trade: more of the security is Aries's own code, and that code is on the internet. What bounds it: Go's TLS and HTTP server; one listener with few paths; no page that renders user content; every limit of [§4.6](#46-limits-rule-18); and a review before 1.0 ([§12.4](#124-what-a-security-review-must-look-at-before-10)).

### 12.2 Threats

| Threat | What the attacker gets | What bounds it |
|---|---|---|
| A stolen `aries` token (a log, a proxy) | Nothing without the DPoP key | DPoP (**Measured**); tokens never logged; the 401 body never echoes one |
| A stolen token and key (the user's files, or a backup of them) | The principal for the rest of the day | The day; `aries admin signin end`, `principal disable`; the keystore in 1.x |
| A token replayed from another machine | Nothing | The proof's key must match the grant's (**Measured**) |
| A proof replayed | Nothing | `jti` remembered for its window; `htu`, `htm`, `ath` checked (**Measured**) |
| A stolen connector token (bearer) | The person's `mcp` scope on one project for up to an hour | Short life; audience per project; no admin, no gate; refresh reuse ends the grant |
| A phished sign-in | The victim's principal for a day on the attacker's machine | Invitations only; `prompt=login`; the device grant off; sign-ins announced to the inbox; identity.md §13 |
| The listener on the internet: scanning, floods, slow clients | Load | §4.6's limits; `ReadHeaderTimeout`; 401s rate-limited per address; nothing served unauthenticated but metadata and the sign-in's redirects |
| TLS downgrade, weak ciphers | Nothing | TLS 1.2 minimum, Go's suites, HSTS |
| CSRF against the OAuth endpoints | A code for the attacker's session in the victim's browser | `state` checked by `aries`; PKCE; `iss` (RFC 9207); the authorization request is GET with no side effect until the provider returns |
| A cross-site page driving `/mcp` from a person's browser | Nothing | `Origin` checked (**Measured**); tokens only in headers, never cookies, so a browser cannot be made to send one |
| DNS rebinding against `aries login`'s loopback listener | A code delivered to a page | The listener binds `127.0.0.1` only, answers one path once, checks `state`, and the code needs the verifier; it never serves anything else |
| A malicious MCP client registering (DCR) to phish | A consent page in Aries's name | The consent page shows the client's name as unverified and the redirect's host; DCR off on air-gapped profiles; CIMD from allowed hosts only |
| SSRF through CIMD fetches | Requests from the server to internal hosts | Allowlisted hosts, public addresses only, no redirects (**Measured** the Go SDK applies the same guard as a client, [RESULTS.md](../../spike/https/RESULTS.md#5-the-remote-mcp-endpoint)) |
| A compromised proxy in front of ariesd | Everything it sees | `aries` tokens are DPoP-bound; the operator's proxy is the operator's trust; smart cards need TLS at ariesd |
| The host key of SSH, or the TLS certificate, substituted | A man in the middle | SSH: the pinned host key. HTTPS: the system's or the site's CA, `server.ca` per server; pinning the certificate is not offered (renewals would break it) |

### 12.3 What is new to log and count

Sign-ins, ends and refusals by provider and reason (never by token or address at info), connections by transport, token checks by result, DPoP refusals by reason, connector registrations. Each joins `internal/logs`' schema with its kind (rule 2).

### 12.4 What a security review must look at before 1.0

1. The authorization server: codes (single use, one minute, bound to client and redirect), PKCE, the redirect checks, state, `iss`, refresh rotation and reuse, the device grant's page.
2. The token check on every path: `/v1/connect`, `/mcp`, `/oauth/revoke`; scope enforcement at dispatch for every op, by a test that walks the op list.
3. The `ws` package and its fuzzing; the in-process hand-off into `Server.handle`.
4. The provider callback: the server-side exchange, the verifier (identity.md §6.4), binding by verified address once only.
5. CIMD and DCR: SSRF, the consent page, pruning.
6. The listener's limits under load, and what a flood of unauthenticated requests costs (a soak with the partition test's shape).
7. Behind a proxy: what is trusted from it, and that nothing is trusted from `Host` or `X-Forwarded-*`.
8. Secrets: every new file 0600, nothing in argv, logs or spans (rule 2), the client secret's handling.
9. The offline profile: that nothing reaches outside (a test on an internal network in CI, as the spike ran).
10. SSH off by default: that a default install exposes no SSH path.

## 13. Horizontal scaling

Decision 79 and [spike/postgres §4.2](../../spike/postgres/RESULTS.md#42-several-ariesd-processes-on-one-postgres) hold: one daemon writes a project, and several daemons on one Postgres are placement, each the writer of different projects.

- **1.0: one active daemon, and a standby.** A load balancer in front does TLS and health checks; it sends everything to the active daemon. HTTPS changes nothing here but making the load balancer ordinary.
- **Sticky by project, when a server outgrows one daemon (1.x).** Every path that reaches a project names it: `/v1/p/PROJECT/connect` (1.0 reserves it beside `/v1/connect`, whose hello names the project), `/mcp/p/PROJECT`. A load balancer routes by the path to the daemon that holds the project, each daemon holding its projects by a Postgres advisory lock (spike/postgres §4.2). A daemon that receives a project it does not hold answers 421 Misdirected Request with the holder's address, and `aries` reconnects.
- **What must be shared across daemons:** grants and tokens (in the control schema already), and the DPoP `jti`s seen (a proof replayed to another daemon within its two minutes would pass a per-daemon cache): with several daemons, the server issues DPoP nonces (RFC 9449 §8) or keeps the `jti`s in Postgres. The OAuth endpoints run on any daemon; sign-ins in progress move into the control schema.

## 14. Settings, refusals and the protocol

**Settings** added to `ariesd.yaml`, beside identity.md §10.1's `login:` and `workload:` (which keep their keys, `token_lifetime` and `max_token_lifetime` among them, one pair in both, decision 88; `login.enroll` goes):

```yaml
transports: [https]                 # add ssh to serve the bridge
https: { … }                        # §4.2
login:
  providers: [ … ]                  # §8.2: discovery fetch|static, ca_file, jwks_file
  device_grant: false               # Aries's own (§6.2); decision 55
mcp:
  enabled: true                     # the remote endpoint
  cimd_hosts: [claude.ai]           # whose Client ID Metadata Documents are fetched; [] for none
  dcr: true                         # dynamic registration; false on air-gapped
  static_clients: []                # pre-registered: id, name, redirect URIs
  access_lifetime: 1h
  origins: [https://claude.ai]      # browser origins allowed at /mcp
```

**Refusals** (rule 11), beside identity.md §10.3's:

| Case | Code | Fix |
|---|---|---|
| No token, unknown or revoked | `auth` | Run `aries login` |
| Token's sign-in ended | `auth` | Run `aries login` |
| DPoP proof missing, wrong key, replayed | `auth` | Run `aries login` (the token file and key no longer match) |
| A scope the op needs (`cli` for a gate pass) | `forbidden` | Ask a person to run `aries gate pass` in a terminal (rule 12) |
| SSH off (bridge) | `unavailable` | An operator sets `transports: [https, ssh]` and runs `install-server.sh --enable-ssh` |
| SSH off (seen by `aries`) | `auth` | Use `server.url`, or ask the operator |
| Sign-in over SSH | `invalid` | Set `server.url` |
| The WebSocket upgrade refused by a proxy | `unavailable` | The proxy must pass WebSocket upgrades on `/v1/connect` (the guide's section) |
| TLS certificate does not verify | `auth` | Ask the operator for the site's CA (`server.ca`) |

**The protocol.** No frame changes. New ops join protocol 8 (decision 78): `signin.list`, `signin.end` (admin), `transport.status` in `status`' result. The bridge frame is unchanged.

**CLI.** `aries login [--provider P] [--url | --device]`, `aries login --workload KIND`, `aries logout`, `aries whoami` (transport, provider, end); `aries admin signin list NAME | end ID`; `aries admin client list | remove ID` (OAuth clients); `install-server.sh --profile P --enable-ssh --disable-ssh`.

## 15. Plan

After P1 and P2. Each slice leaves `main` working and off until configured. Estimates are one engineer's weeks, from the size of comparable slices and the spikes' code (spike/https: about 2,900 lines of commands, and 780 of packages with 540 of tests), not from a breakdown. **Air-gapped SSO is in the first identity slices, not the last**: T2's gate is a sign-in on an internal network with Keycloak and a key set file.

| Slice | Delivers | Was | Gate | Estimate |
|---|---|---|---|---|
| **N0** | Real-tenant spikes as identity.md §14 had them (Google, Entra commercial, Okta, GitHub App, GitHub Actions, Google Cloud, Azure), **plus AD FS (Windows Server 2019 or later) and Keycloak federating to an AD domain** (Samba AD in a container suffices for the federation; AD FS needs a Windows Server VM); the providers' questions of §8.3 and §8.5. No GCC High, DoD, Secret or Top Secret tenant (deferred by the maintainer). The way-in spike under AppArmor is dropped | Carried, changed | Each provider's row confirmed or corrected in RESULTS.md | 1.5 weeks, with the maintainer's registrations and a Windows Server |
| **N1** | Identity: `kind`, display name, owner, identities, invitations, the admin commands | Carried | identity.md §14's | 1.5 weeks |
| **T1** | The HTTPS listener: `transports` with SSH off by default; TLS by files, ACME and proxy; HTTP/2; the limits; `internal/wsconn`; the in-process hand-off into `Server.handle`; `/.well-known/aries`; `server.url` and `server.ca` in `aries`, `client.Unreachable` for HTTP; status; AppArmor, systemd and the installer's profiles, `--enable-ssh`, the tests that a default install exposes no SSH path | New | e2e over HTTPS beside SSH (the same suite, both transports); the soak once over HTTPS; the installer test; the Docker test server on HTTPS | 2.5 weeks |
| **T2** | Aries's authorization server and sign-in: grants, tokens, DPoP, `/oauth/*`, the callback with server-side exchange, the verifier's offline mode (`discovery: static`, `jwks_file`, `ca_file`), the `entra` provider with a configured authority, `aries login` (loopback, URL), `logout`, `whoami`, token storage, scopes at dispatch, revocation and the close at the end | Replaces N2 (most), N3, N4 | An e2e sign-in against a fake issuer in process; **the spike's offline run in CI**: Keycloak on an internal Docker network, a sign-in and a token check with no route out, by key set file; fuzz targets for the token and DPoP parsers | 3 weeks |
| **N5** | Service principals: trust rules, token exchange (RFC 8693), the GitHub Actions, Google and Azure kinds, `aries login --workload`, `docs/ci.md` | Carried, changed | identity.md's, with the exchange in place of the key; a real Actions run (manual) | 1.5 weeks |
| **T3** | The remote MCP endpoint: `/mcp/p/PROJECT` in `ariesd` with `internal/mcpserver`, protected resource metadata, CIMD with its allowlist, DCR bounded, static clients, refresh rotation, origins, connector sessions and lease renewal by tool call, the consent page | New | The Go SDK's client through the flow in an e2e test; Claude Code against the Docker test server (manual); a claude.ai connector against a public test server (manual, the maintainer) | 2 weeks |
| **S1** | SSH as the second transport: `key add --expires`, `expiry-time` with `Z`, the handshake's check and the close at the end, the bridge refused when off, turning SSH on and off | Replaces N2's rest | identity.md N2's server test; the Docker smoke test with SSH enabled in a zone other than UTC | 1 week |
| **N6** | GitHub's adapter for people; Aries's own device grant | Carried, changed | Tests against a fake GitHub API; a manual device sign-in from a phone | 1 week |
| **N7** | The Desktop extension, thinner (§10) | Carried, smaller | The manifest test; a manual sign-in from Claude Desktop on macOS and Windows | 0.5 week |
| **N8** | The guides (server: profiles, TLS, proxies, providers inside the network, AD federation through Keycloak, cards at the provider, SSH on and off; agents: connectors and Claude Code; ci; security), the Docker test server with Keycloak and nginx, the threat model, and the security review of §12.4 | Carried, larger | The review's findings fixed or recorded; the smoke tests pass in both profiles | 2 weeks |

**About 16.5 weeks** in all, against identity.md's 11 to 12: the listener and the pipe (2.5), the authorization server's share beyond the old enrollment (about 1), the MCP endpoint (2) and a larger review are new; the enrollment path, its AppArmor spike and SSH sign-in (about 2) are gone. Order: N0 can start now. N1 and T1 are independent; T2 needs both; N5, T3, S1 and N6 need T2 and are independent of each other; N7 needs T2. With N1 to N7 beside the port's P4 and P5 where their files do not overlap (decision 85), as before.

**What could move to 1.x to bring it back toward 12 weeks**, each a question of §16: the remote MCP endpoint (T3, 2 weeks), GitHub's adapter and the device grant (N6, 1 week), and ACME (half a week of T1).

### As built, 16 Oct 2026: slice T1

**Decided 16 Oct 2026 (aries.md decision 89):** the proof by SSH key stays after T2, beside sign-in's tokens; T2 adds tokens to the same upgrade request and keeps the key proof as a way in.

T1 is on its branch, in the order of its row; where it departs from §3 to §11, this says so.

- **How an HTTPS connection authenticates before T2: a proof by an SSH key, over the WebSocket** (`internal/keyproof`). §6's tokens do not exist yet, and T1 had to open no path that skips authentication. Of the two ways the slice's brief offered, refusing every connection until T2 behind a test-only authenticator would have left a default install unusable for a slice and the Docker server untestable over HTTPS; the proof uses what servers already have, the keys of the control store and of `authorized_keys`. After the upgrade the client offers the keys it would offer sshd (decision 35's choice, at most `keyproof.MaxKeys`, 16); the server answers with the index of the first it knows and 32 random bytes; the key signs a transcript of a domain string, the server's URL as the client has it, the nonce and the public key, each length-prefixed; the server verifies, checks the URL against `public_urls` (never `Host`: the spike's nginx finding, built for now as §4.2 asks of DPoP), and hands the connection to `Server.handle` with the key's principal and, for a key of the store, its fingerprint, exactly as a bridge frame names them. So the handshake's checks, the limits behind it and the close when a key is removed, a principal disabled or a membership ended are the SSH path's own. The proof is fresh per connection, so no replay cache is kept; RSA must sign with SHA-256 or SHA-512. A refusal is a welcome frame carrying a `proto.Error`, which `aries` already reads; the offer, challenge and proof are messages of the WebSocket before the hello, not frames of the protocol, so §5.4's "no frame changes" holds and protocol 8 is untouched. **Recommended after T2: keep it**, as a way to sign in beside tokens, on by default with a setting to turn it off once a site signs everyone in through its provider: it serves air-gapped teams with keys and no identity provider, and service principals with no workload identity (decision 87 item 9's reason for keeping key admin), and leaves T2 nothing to undo. T2 then authenticates the upgrade itself (`Authorization` and `DPoP` headers, §5.4 step 1), and a connection that brings neither goes to the key proof.
- **The listener** (`internal/server/https.go`): `Config.HTTPS`, served by `Serve` beside the socket and stopped with it; `/.well-known/aries` (version, transports, `auth: ["key"]`) and `/v1/connect`, everything else 404 with a `proto.Error`; HSTS, `nosniff` and `no-store` on every response; HTTP/2 offered for all but the WebSocket; a request with `Origin` refused 403 (a browser's page). The limits of §4.6 that T1 needs are `server.Limits`, with the naming rule of decision 88: `https_conns_per_server` (§4.6's `https_conns`), `https_conns_per_addr`, `https_header_bytes`, `https_header_timeout`, `https_body_bytes`, `upgrade_timeout` (to the end of the proof; the hello then has `helloTimeout`, as over SSH), `ws_message_bytes`, and `auth_failures_per_minute` with `auth_failure_burst` (§4.6's `auth_failures_per_addr`, 30 a minute, burst 60). Behind a proxy the per-address limits do not apply: every connection is the proxy's. The rest of §4.6 is T2's and T3's. Pings every 25 seconds (`HTTPS.Ping`); `idle_timeout` counts frames.
- **TLS** (`internal/server/tls.go`): `files`, read again at the first handshake after either file changes (no SIGHUP handling: the change itself is the signal, and `ariesd` has none); `acme` by `autocert`, its cache 0700, made if missing; `proxy` with `trusted`. `min_version` `"1.2"` or `"1.3"`. `client_ca` (cards, 1.x) is not a setting yet. The certificate's end is in the status report for `files`, not for ACME.
- **`transports:`** defaults to `[https]`; `[https, ssh]` and `[ssh]`. `ariesd serve` refuses to start with HTTPS on and `https:` unset, naming both fixes; the admin commands need none of it. With SSH off: no keys file, and one ariesd wrote is removed (`keys_file.removed`); the bridge refused `unavailable` with a fix naming `transports: [https, ssh]` and `--enable-ssh`; `ariesd status` and the upgrade's probe still use the socket; `aries admin key add` succeeds with a notice that the key signs in over HTTPS and that sshd takes it once SSH is on (§11.1's wording assumed keys did nothing with SSH off; with the key proof they sign in). The status report gains `transports:` (`proto.Transports`, a result field, no bump). Turning SSH on and off at runtime with `systemctl reload` is not built: a restart reads the setting. `install-server.sh --disable-ssh` is not built; the guide says what to remove.
- **A server of one project** checks a key proof against the lines of `authorized_keys` that force `…/ariesd stdio --principal NAME` with nothing more, which `ariesd serve` reads when HTTPS is on (`authorized_keys:`).
- **`internal/wsconn`** is the spike's `ws`, hardened: a length refused unless in its shortest form, at most 64 control frames inside a message, UTF-8 text and close reasons, close codes 1002, 1007 and 1009 sent for what was refused, a sticky read error, the response's `Upgrade`, `Connection` and no extension or subprotocol checked on the client, a `TLSError` told apart from a network failure, and a `Close` that does not wait on a writer stuck behind a peer that reads nothing. `FuzzRead` ran 14.8 million inputs in 60 seconds with no finding; `keyproof`'s two parsers have fuzz targets too.
- **The client**: `server.url`, and `server.ca` as PEM or a PEM file inside the repository (the confinement of `key:`); `host` and `host_key` are needed only for SSH. `ARIES_TRANSPORT=https|ssh`; `transport: ssh` in the user's own config (§5.7) is not built, the variable serving. `client.Unreachable` as §5.6 has it, a 4xx from a proxy and a URL with no `/v1/connect` among what is not queued. The outbox and `seen.json` are keyed by the URL over HTTPS (§5.6's "the first of `public_urls` the welcome names" would need a welcome field, so the client's own URL stands in). `aries admin project add` prints the URL and CA of the admin's own config, since the server sends no URL.
- **Operations**: `install-server.sh --profile`, `--enable-ssh` and the options of §3, with `--plan`, which prints what a profile serves and writes before the machine is checked, and which the installer's test holds each profile to; SSH's steps only with SSH on, the first admin's key otherwise in `/etc/aries/admin.keys` for `control init`; drop-ins for `CAP_NET_BIND_SERVICE` below port 1024 and for ACME's cache; ufw opened when active. AppArmor: `capability net_bind_service`, the TLS files and the ACME cache in `ariesd-serve`, whose blanket capability deny became a list (it would win over the allow); `admin.keys` in `ariesd-admin`. The `ariesd` (bridge) profile is still installed with SSH off, one file holding all three; nothing runs under it then but `ariesd status`.
- **The gate**: the e2e suite runs on both transports (`ARIES_E2E_TRANSPORT=https`, in the gate and CI), through a front in the test that stands where an operator's proxy would and relays each WebSocket message to the daemon's own TLS listener; tests of SSH itself are pinned to SSH. The soak ran once over HTTPS, 10 minutes of work: 198,510 requests in 600.5 seconds, every check passed. The Docker test server serves HTTPS with a test CA; its smoke test passed over HTTPS (146 checks) and over SSH with SSH on (156). `TestDefaultInstallExposes` (`cmd/ariesd`) holds a default install to §12.4 item 10, and to no unauthenticated path.
- **Not built in T1**, as planned: tokens, DPoP, sign-in, `/oauth/*`, `/mcp`, `/v1/p/PROJECT/connect`, HTTP metrics (none was added to `internal/telemetry`), and the review of §12.4. Not measured: the listener under thousands of connections or an unauthenticated flood; ACME against any ACME server; AppArmor under an enforcing kernel (parsed with 4.0.1 only).
- **The listener's metrics, after T1 (16 Oct 2026):** the connections it holds and those it refused, upgrades and key proofs by how they ended, TLS handshakes that did not complete and the certificate's reloads, with fixed values only; [aries.md §16, As built](aries.md#16-observability) and the guide's [What is measured](../server.md#what-is-measured) list them.

### As built, 16 Oct 2026: slice N1

N1, the identity tables, invitations and their admin commands, is on its branch; [identity.md §14, As built](identity.md#as-built-16-oct-2026-slice-n1) says what it built and what T2 inherits of it.

### As built, 16 Oct 2026: slice T2

T2 is on its branch, in the order of its row; where it departs from §4 to §8 and §12 to §14, this says so.

- **The verifier** is the spikes', moved and hardened, on the standard library alone (`internal/jose`, `internal/oidc`): RS256 and ES256 only, `none`, the HMAC family and any `crit` refused, a key used only under the algorithm its type is for, RSA under 2,048 bits refused; issuer exact, the client id among the audiences and `azp` required to be it when there are several; `exp`, `nbf`, `iat` within `skew` and `max_token_age`, the nonce, and `auth_time` within `fresh_auth`, on the store's clock. A refusal is one of four kinds (`ErrInvalidToken`, `ErrStale`, `ErrUnavailable`, `ErrNotAllowed`), which the server words. `discovery: fetch` reads the discovery document at the first sign-in, not at start, so that a provider that cannot be reached when the daemon starts delays nothing, and again after a failure at most once a `jwks_refetch`. `discovery: static` writes every endpoint out, with `jwks_file` (nothing fetched to check a token) or `jwks_url`. A key set file is read again at the first token of a key it lacks, at most once a `jwks_refetch`: no SIGHUP, as T1's certificate files, the change being the signal. `ca_file` replaces the system's roots for that provider; a provider's redirect is never followed; each request is bounded to 10 seconds and 64 KiB. Plain http is allowed only to `localhost`, a name under `.localhost` and a loopback address, for a test provider: the Docker test server's Keycloak, which serves http. `entra` takes `authority`, `tenant` and `token_version`, with no default authority, refuses `common`, `organizations` and `consumers`, checks `tid` and `ver`, takes `oid` as the subject, and counts an address verified only by `xms_edov`. Beside §8.2's settings: `require` (claims with exact values, `hd` for Google) and `domains` (the address's domain), the brief's "allowed domain or tenant"; `prompt_login: false` for Google. A test holds `internal/oidc` to naming no provider's host. `jose.FuzzParse`, `oidc.FuzzVerify` and `dpop.FuzzVerify` are fuzz targets: 10.3 million, 1.4 million and 4.0 million inputs in 20 seconds each, with no finding.
- **The authorization server** (`internal/server/signin.go`) is §6.2's, with these departures. `aries-cli` is a constant client; there is no `oauth_clients` table, which is T3's with registration. aries asks for `cli mcp`, or `mcp` alone; nothing issues `ci` before N5. The provider's redirect is the first of `public_urls` and `/auth/callback`. A refusal after the provider (no invitation, an address not verified, a sign-in not fresh) rides the server's code back to aries, which hears it at the token endpoint as `access_denied` with the protocol's code and fix beside OAuth's error, so that the person learns why in the terminal, not on a page; the browser is told to return to the terminal. A request that is not aries's (another client, a redirect that is not loopback `http` on a port) gets a page and never a redirect (RFC 9700 §4.11). Sign-ins in progress (10 minutes) and codes (a minute, once) are the daemon's memory, bounded together by `signins_pending_per_server` (§4.6's `signin_pending`, named by decision 88's rule); a restart costs a person a second `aries login`. `/oauth/revoke` takes the token alone, as RFC 7009 has it for a public client: whoever holds a token may end it. The device grant is not built: it is N6's, as §15 has it, with GitHub's adapter, and T2 was already the largest slice; so `device_grant` is not a setting yet.
- **Tokens** are two `crypto/rand` texts, 52 characters of base32 and 260 random bits, kept as their SHA-256 in hex. §6.9's tables are control migration 0004: `grants` with `end_reason` beside `revoked_at` (`logout`, `revoked`, `identity_removed`), `tokens` with `grant_id` (`grant` is SQL's word), and `kind` and `used_at` for T3's refresh tokens; no `trust_rule` column, which N5 adds with its table. A grant ends at its end or is ended, and is deleted, quietly, `prune_after` after (1 hour by default). The control store's index holds the live tokens by hash, so a connection presenting one costs a memory lookup, not §6.3's read cached for a minute; ending a grant drops it from the index in the same write. `grants_per_principal` (64) bounds live sign-ins; a new `aries login` on a machine ends that machine's last one, so a person's do not pile up.
- **DPoP** is on the upgrade to `/v1/connect` and at the token endpoint, not per request: the proof names each of `public_urls` with the path, never `Host` (the same rule as T1's key proof), within a minute either way of the store's time, for the token (`ath`), once. The proofs seen are the daemon's memory, two minutes each, bounded by `dpop_proofs_per_server` (§4.6's `dpop_proofs`); several daemons would each hold their own, and a proof replayed to another within its minute would pass: they would share the proofs seen in the control schema, or issue DPoP nonces (RFC 9449 §8), as §13 has it. 1.0 runs one daemon. *16 Oct 2026, issue #160:* a proof is checked without being remembered (`dpop.Check`), and remembered (`Verifier.Spend`) only once its request has earned it, since anyone can sign proofs with a key of their own and the memory is bounded: at the token endpoint after the code is found live, for `aries-cli` and its redirect, with its PKCE verifier, and not for a code whose sign-in was refused (no token comes of it); at `/v1/connect` after the proof's key is the grant's (`Verifier.Verify` takes the grant's `jkt`). The code is spent whatever comes of the request. Before, about 830 anonymous token requests a second filled the 100,000 and refused every sign-in and connection `busy`. Refusals at the token endpoint now cost the address under `auth_failures_per_minute`, as refused key proofs and tokens do, and an address past it is refused there 429 too. The two endpoints keep one memory: what each may now remember is bounded by people who signed in at the provider (a code) or hold a token and its key, so neither endpoint's anonymous traffic can push out the other's, and a second limit would buy nothing; a signed-in person who floods connects with fresh proofs can still fill it, and a separate memory would not keep them from locking out every other connection; that is a person with access, whose sign-in an admin ends (`aries admin signin end`), and no anonymous caller. Forgetting is in the order proofs were spent, which is the order their windows end on the store's time (rule 19), so a full memory refuses in constant time rather than walking every proof under its lock.
- **Binding** is N1's rule: a sign-in matches `(issuer, subject)`; else the open invitation at the provider whose address, compared as N1 does, is the token's verified address binds, by compare-and-swap, with an `identity.bind` event, in the same write as the grant; a lapsed invitation, an unverified address, an address bound to another subject, and a disabled principal are each refused, with the person's own address in the refusal and in no log line. Later sign-ins move `last_sign_in` with `wtx.touch`. The write is the principal's, named once the match is known. `identity.remove` ends its sign-ins in the same write and closes their connections. `invite.add` and `principal add --invite` check the provider against `login: providers` when there are any; a server with none takes any name, as N1 did, since nothing signs in there.
- **Each sign-in is announced** (identity.md §13; issue #166, from T2's security review, 16 Oct 2026). An inbox is a project's, and a grant is the control store's, so once the grant has committed the daemon puts a `signin.granted` item, from `ariesd` and for every session, in the principal's inbox in each active project it is a member of, before it answers the token: `aries inbox`, prime and the push show it as any item. A project's closed store is opened for it through the registry, as the archive does; an archived project is passed over. The item names the provider, the flow, the client, the machine the client said, when the sign-in ends and `aries admin signin end ID` and `aries logout`; never an address, a subject, an issuer, a token or an IP address, and the machine last, so that the cut to 200 bytes takes it first. `notices_per_minute` never drops it; `unread_per_principal` and `inbox_keep` bound it as every item, and the sign-ins themselves are bounded by `grants_per_principal` and a provider's sign-in each, so no new limit. It is not in the control store, where it would be atomic with the grant, because a server-level notice would need its own read, ack and push beside the inbox's, for one kind. So it is not atomic: a store that cannot be opened or written is logged (`signin.announce_failed`) and passed over, the sign-in standing and the audit stream having it, and a principal in no project is told nothing. The project's write records no event (rule 7's `wtx.quiet`): the history is the control store's `signin.grant`, and an event in the project's log would show every member the sign-in.
- **Scopes at dispatch** are a table, `opScopes`, of the scopes besides `cli` each op is open to; a test reads every `Op` constant of `internal/proto` and fails until each is in it, so a new op is a person's terminal's until decided. `mcp` is the ops the MCP tools send; `ci` a member's ops and a ci gate's report, never a waive or an admin op; neither takes the session `cli`, and `mcp` never `ci`. A `ci` sign-in's `gate.pass` of a person's gate is refused by the store's session rule (decision 68), which a test holds; one of a ci gate goes through, as from the session `ci` today. A connection that proved a key is held to its principal's role alone.
- **The close** (decision 59): a timer on the server's own clock, from the store's time, closes a connection at its sign-in's end with a last frame, an answer to no request, `auth`, `your sign-in ended at …`, fix `aries login`, which aries takes for why the connection ended; `/oauth/revoke`, `signin.end` and `identity.remove` close a sign-in's connections the same way at once. The handshake also refuses a sign-in that ended while it ran. An agent told `aries login` is told instead to ask the person, never to run it (`proto.FixSignIn`).
- **The client.** `aries login` listens on `127.0.0.1`, opens the browser where it can (macOS and Windows outside SSH, Linux with a display) through `cli.Env.OpenBrowser`, which only `cmd/aries` sets, and otherwise prints the URL and takes back the pasted address while it still listens; it checks the state and `iss`. The sign-in is `signin/SERVER/token.json` and `dpop.key` (PKCS #8) in the data directory, 0600 in 0700, refused if anyone else may read them. Every `aries` and `aries mcp` that reaches the server over HTTPS presents a live sign-in, and proves a key otherwise (decision 89); a refusal of every key says `aries login` is the other way in. `aries logout`, `whoami` with the sign-in, `aries admin signin list NAME` and `end ID` (§14's `signin list`). The outbox takes a live sign-in's principal for whose its writes are.
- **Protocol 8** (no bump, decision 78): the ops `signins` and `signin.end` (decision 88's bare plural for §14's `signin.list`), and the status report's `signin`. The welcome is unchanged. The sign-in's paths are HTTP, not the protocol.
- **Settings**: `login:` gains `token_lifetime` (a minute to `max_token_lifetime`), `max_token_lifetime` (at most 7 days), `fresh_auth` (a minute to an hour), `max_token_age`, `skew` (at most 5 minutes), `jwks_refetch` (at least 10 seconds), `prune_after` and `providers`; `ariesd serve` refuses providers on a server of one project or with HTTPS off. Logs: `signin.granted`, `signin.refused`, `signin.ended`, `signin.provider_failed`, and the audit stream's `audit.identity.bind`, `audit.signin.grant` and `audit.signin.end`; no line holds a token, a code, a verifier, an address or a subject. Telemetry: the reason `signin` of a dropped connection and the three limits; token checks on `/v1/connect` are counted, as #156 counts key proofs, by `aries.https.tokens` (`result`: `ok`, or the refusal's fixed word, `token_*` for the token and `dpop_*` for its proof, never the token, its hash or the proof's key), and a refused one as an upgrade refused with reason `token`; §12.3's counts of sign-ins by provider are the log's lines, not instruments yet. The status report shows each provider, where its endpoints and keys come from, the key ids and when read, and the live sign-ins. AppArmor reads `/etc/aries/login`.
- **The gate.** The CI's offline sign-in is the in-process stand-in issuer with a key set file (`oidctest`; `TestSignInOffline`, `TestProviderOffline`), which counts no fetch of the discovery document or the keys, not Keycloak: Keycloak in CI needs Docker in the test job, a 750 MB image and half a minute to start, which the job does not run. The real flow ran against Keycloak in the Docker test server (`ARIES_SMOKE_IDENTITY=1`), its provider offline, keys from a file: bob signed in by `aries login --url` with curl as the browser, worked, and signed out; alice, uninvited, was refused. That run's network publishes ports to the host for the host's `aries`, so it is not marked internal; T1's guide has the internal-network run of Keycloak by hand. End-to-end tests sign in through the world's front, which now proxies the sign-in's paths and passes the upgrade's headers, on both transports' runs.
- **A disabled principal's key over HTTPS** departs from T1's "the SSH path's own": the key proof finds a key of the control store whose principal is disabled, has it proved, and refuses it `forbidden`, `NAME is disabled on this server`, with the fix to enable, as the handshake refuses that principal's token; it is not refused as a key the server does not know, and is not charged to `auth_failures_per_minute`, which counts failed authentication only. Charged, a disabled person's agents, still trying, emptied their address's bucket within a second and shut everyone else at that address out, an office behind one NAT, or every agent of the soak, which runs on loopback (key proof result `disabled`). A proof of such a key that does not verify is still `bad_proof`, and charged. Over SSH sshd still refuses the key, its line gone from the keys file.
- **Not built in T2**, as planned: the device grant and GitHub's adapter (N6), token exchange and `ci` sign-ins (N5), bearer tokens, refresh, client registration and `/mcp` (T3), `aries admin client`, the keystore (1.x), cards at ariesd (1.x), §8.2's status line for a file's read time beyond the key ids', and the security review of §12.4.

## 16. Decisions for the maintainer

**Decided 15 Oct 2026** (aries.md §14, decision 87): the maintainer accepted all eighteen recommendations below as written, and kept the whole plan in v1.0.0.


Each with a recommendation.

1. **Where the design lives.** *Recommended:* this document for transport and tokens, identity.md for identity, each pointing to the other. *Cost:* two documents to read for sign-in.
2. **Aries as its own authorization server, with opaque tokens.** *Recommended:* yes (§6.1, §6.3). *Cost:* an authorization server to build and review. *Of delegating:* Google and GitHub out, connectors registering at each company's provider, and revocation at `exp`.
3. **A WebSocket per connection as the one pipe, with no fallback in 1.0.** *Recommended:* yes (§5.2). *Cost:* a client behind a proxy that breaks WebSockets uses SSH or nothing. *Of adding request/response with SSE as a fallback:* about 2 weeks and a session that outlives its connection.
4. **Aries's own WebSocket code, no dependency.** *Recommended:* yes (§5.3). *Cost:* 354 lines to own and fuzz.
5. **DPoP for `aries`'s tokens; bearer for connectors.** *Recommended:* yes (§6.4). *Cost:* a signature per request off the WebSocket; connectors' tokens are bearer for an hour.
6. **Lifetimes: `aries` one token for the day and no refresh; connectors an hour with refresh to the day's end.** *Recommended:* yes (§6.5). *Cost:* a connector holds a refresh token for the day.
7. **The token in a 0600 file in 1.0; the OS keystore in 1.x.** *Recommended:* yes (§6.6). *Cost:* a stolen disk gives a day.
8. **Scopes `cli`, `mcp`, `ci`, enforced at dispatch.** *Recommended:* yes (§6.8). *Cost:* every op gets a scope, and a test holds the list.
9. **SSH key admin commands allowed while SSH is off, with a notice.** *Recommended:* allowed (§11.1). *Cost:* keys that do nothing until SSH is on.
10. **HTTPS may be turned off (`transports: [ssh]`).** *Recommended:* yes (§11.1). *Cost:* a profile with no sign-in to document.
11. **Expiring SSO-minted SSH keys: not in 1.0; `key add --expires` in 1.0; the published enrollment key dropped.** *Recommended:* yes (§11.3). *Cost:* a team that must use SSH and wants SSO waits for 1.x.
12. **Active Directory without OIDC: require an OIDC front (Keycloak or AD FS).** *Recommended:* yes (§8.3). *Cost:* a site with only AD runs Keycloak.
13. **Smart cards at the provider in 1.0; mutual TLS at ariesd in 1.x; cards' SSH keys today.** *Recommended:* yes (§8.4). *Cost:* a site that wants cards checked by Aries itself waits.
14. **The remote MCP endpoint in 1.0.** *Recommended:* yes, as T3. *Cost:* 2 weeks, `internal/mcpserver` and the SDK linked into `ariesd`, and an endpoint on the internet. *Of 1.x:* Desktop and claude.ai users install the extension meanwhile.
15. **Client registration: CIMD with an allowlist, DCR bounded, static clients.** *Recommended:* all three, DCR off on the air-gapped profile (§9.3). *Cost:* three paths to test.
16. **The Desktop extension kept, thinner.** *Recommended:* yes (§10). *Cost:* a bundle to ship beside the connector.
17. **Aries's own device grant, off by default, in place of the provider's.** *Recommended:* yes (§6.2). *Cost:* the device page is Aries's to defend.
18. **Profiles in the installer (`internet`, `private`, `airgapped`, `ssh-only`).** *Recommended:* yes (§3). *Cost:* four paths in the guide and the installer's test.

## 17. Not established

- **Claude's clients**: whether they send DPoP (the pages read name none, so the design assumes not); which MCP revision claude.ai and Claude Desktop speak; whether Claude Code applies an SSRF guard like the Go SDK's to a server on a private address (the Go SDK does by default, **Measured**, and an internal-network deployment of a client built on it must opt out); what Claude Desktop does with a connector whose server's certificate is from a private CA (it connects from Anthropic's cloud, so presumably it refuses). None of this was tried with Claude: [RESULTS.md, section 8](../../spike/https/RESULTS.md#8-trying-the-spike-as-a-connector-in-claude-desktop) is the maintainer's to run.
- **AD FS**: the nonce in the code flow's ID token, `max_age` and `auth_time`, and the ID token's claims for a smart-card sign-in. N0.
- **Microsoft's Government cloud**: the issuer's exact form, managed identities and federated credentials there. **GCC High, DoD, Secret and Top Secret are not tested, deferred by the maintainer**; 1.0 claims them as configurable and untested.
- **Corporate TLS-inspecting proxies and WebSockets**: not measured; only nginx was.
- **The cost in `ariesd`'s binary and start** of linking `internal/mcpserver` and the SDK, and whether the bridge's `TestInitAllocations` still holds.
- **Load**: the listener under thousands of connections, and a flood of unauthenticated requests; the spike measured one client.
- **ACME** with an internal ACME server: designed, not run.
- **Several daemons sharing DPoP replay state**: designed, not built.
- **Costs to build**: estimates, not a breakdown.
