# HTTPS transport spike: results

**Verdict: Aries's frames carry over HTTPS unchanged on a WebSocket, through a buffering reverse proxy, faster than SSH; a sign-in through an internal Keycloak yields a token of Aries's own, bound to the client's key, that the server refuses everywhere it should; and the remote MCP authorization flow ends in a tool call; all on a network with no route out.** HTTP/2 full duplex did not survive nginx's defaults. Three findings change the design ([docs/design/transport.md](../../docs/design/transport.md)): the DPoP check must use the server's configured URLs, not `Host`, which nginx rewrote; the Go MCP SDK's client refuses an authorization server on a private address unless told otherwise; and the Go MCP SDK v1.8.0 already serves MCP 2026-07-28 statelessly behind a bearer check, so the endpoint needs no new dependency.

15 October 2026. Code: this directory, a Go module of its own (`go.mod`) that imports `internal/proto` from the main module and the verifier from `spike/identity`, so the main module and its gate are untouched.

| Path | What |
|---|---|
| `ws/` | WebSocket (RFC 6455) on the standard library: the handshake on both sides, messages, fragments, ping and pong, close, masking per side, a size bound. Table tests and a fuzz target |
| `dpop/` | DPoP proofs (RFC 9449), ES256, made and checked, with the `jti` remembered for its window and a bound on how many. Table tests |
| `provider/` | A provider's settings to a verifier: `oidc` and `entra` kinds, the authority, tenant and token version configured with no default, a CA file, a key set file with nothing fetched. Tests against an Entra-shaped stand-in at a non-commercial authority |
| `cmd/spike/` | The stand-in ariesd (`serve`: HTTPS, the three pipes, the authorization server, `/mcp`), the client (`login`, `workload`, `mcp`, `bench`), the CA (`ca`), Keycloak's realm (`setup`, `jwks`), and `egress`. A stand-in browser fills Keycloak's forms; it is not part of the design |
| `nginx/default.conf` | nginx as an operator would put it in front: TLS, HTTP/2 to clients, the two WebSocket lines, every buffer at its default |
| `run.sh` | Runs all of it in throwaway containers on a Docker network marked internal, and removes them |

To rerun, with Docker and Go on `PATH`: `spike/https/run.sh STATE_DIR`, with a directory that does not exist yet; it holds the CA's keys, the generated passwords, the client secrets and the token, none of which is printed. Delete it afterward. `go test ./...` from this directory runs the unit tests with no containers. The spike also added `oidc.NewStatic` to `spike/identity` (a verifier from a key set file), with two tests.

## 1. Environment

| | |
|---|---|
| Machine | Apple M4 Max, macOS 27.0.1, Docker Desktop 4.94.0 (engine 29.8.2), containers on linux/arm64 |
| Network | `docker network create --internal`: containers reach each other by name and nothing else |
| Keycloak | 26.8.1, `quay.io/keycloak/keycloak@sha256:79c623a22abacf3fe6c00a5fb30f5b7843d717b234033b80995891c06116d244`, `start-dev` on HTTPS only (`--http-enabled=false`), with a certificate from the spike's CA for `keycloak.internal` |
| The stand-in ariesd | `spike serve`, static linux/arm64 build, in `ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55` (24.04): one on `:8443` discovering Keycloak at its internal URL, one on `:8444` with Keycloak's key set from a file and sign-ins of 15 seconds |
| nginx | 1.30.6, `nginx@sha256:970364228b1e4f932abc22d321932816782cca8a20b85a77552a7cfe9acfb3b5` (stable-alpine), as `proxy.internal` |
| sshd | OpenSSH 9.6p1 in Ubuntu 24.04, spike/identity's image (built before the network existed, from Ubuntu's archive), the guide's `Match` block, a key whose line forces `/bin/cat` |
| The client | `spike` in the same Ubuntu image; Go 1.27.2; `github.com/modelcontextprotocol/go-sdk` v1.8.0, `golang.org/x/crypto` v0.58.0 |
| The CA | Made by `spike ca`: ECDSA P-256, certificates for `aries.internal`, `keycloak.internal` and `proxy.internal`, two client certificates standing in for smart cards (alice; mallory, revoked by the CRL) |

Five full runs were made; the figures below are the fourth's. The first three found the faults this file reports as findings, each fixed before the next; the fifth, after a change to logging only, agreed.

## 2. No route out

Every container was on the internal network only. From the client and from the stand-in ariesd, `spike egress` could reach none of `1.1.1.1:443`, `8.8.8.8:53` or `[2606:4700:4700::1111]:443`, and `example.com` did not resolve; from Keycloak, `1.1.1.1:443` was unreachable. Everything below ran so. The one step that reached the internet was building the sshd image, before the network was made.

## 3. The pipes

Each pipe carried `internal/proto`'s frames: a hello and welcome, then `echo` requests, then a `push` asking for 50 `evt` frames 20 ms apart, each stamped with the server's time of sending. Round trips are 300 sequential requests on one connection; "connect" is 20 new connections, each with a full TLS handshake (no resumption), the token and proof checked, the upgrade, and the hello and welcome.

| Pipe | Connect p50 | Round trip p50 | p99 | Push p50 | p99 |
|---|---|---|---|---|---|
| WebSocket, direct | 0.78 ms | 50 µs | 130 µs | 230 µs | 770 µs |
| WebSocket, through nginx | 4.58 ms | 160 µs | 250 µs | 290 µs | 400 µs |
| HTTP/2 full duplex, direct | 5.04 ms (one sample) | 100 µs | 310 µs | 270 µs | 470 µs |
| HTTP/2 full duplex, through nginx | **failed**: no response headers in 5 s | | | | |
| A POST per request, SSE for pushes, direct | n/a | 190 µs | 530 µs | 200 µs | 560 µs |
| The same, through nginx | n/a | 780 µs | 2.0 ms | 300 µs | 1.04 ms |
| The same with `X-Accel-Buffering: no`, through nginx | n/a | 790 µs | 2.44 ms | 350 µs | 860 µs |
| SSH to sshd, forced command `cat` (today's path, no push) | 81.6 ms | 100 µs | 150 µs | | |

- **The WebSocket through nginx, idle for 75 seconds** (past nginx's `proxy_read_timeout` of 60), then used: answered in 1.0 ms. The server pings every 25 seconds.
- **HTTP/2 full duplex through nginx failed** because nginx's `proxy_request_buffering on` (the default) reads the whole request body before it forwards anything, and a stream's body never ends. Direct, Go's server and client did it over HTTP/2 as designed.
- **SSE through nginx worked without `X-Accel-Buffering: no`** in nginx 1.30.6: the events were not held. The round trips through nginx cost more than the WebSocket's because each request is a new HTTP request with a new DPoP proof.
- **SSH's connect is two orders slower** (81.6 ms against 0.78): sshd forks per connection, and the key exchange and the forced command's start are in it. Once connected, the two are alike.
- **A finding: nginx rewrote `Host`.** On the WebSocket's `location`, which sets `Upgrade` and `Connection`, nginx's `proxy_set_header Host $http_host` from the `server` block was not inherited (nginx inherits `proxy_set_header` only into a block that sets none), so the daemon saw `Host: aries.internal:8443` and refused the DPoP proof, whose `htu` named `proxy.internal`. The fix in the stand-in, and in the design: `htu` is checked against the server's configured public URLs, never `Host` (transport.md §4.2).

## 4. Sign-in and tokens

`spike login` made a P-256 key, a PKCE verifier and a state, listened on `127.0.0.1`, and sent the stand-in browser to the server's `/oauth/authorize`. The server sent it to Keycloak with its own PKCE, nonce, `prompt=login` and `max_age=600`; after the sign-in Keycloak redirected to the server's callback; the server exchanged the code with its client secret, verified the ID token (ES256; issuer, audience, nonce, `auth_time`), found alice's invitation, and redirected to the client with an Aries code and `iss`. The client exchanged that with its verifier and a DPoP proof.

| Step or check | Result |
|---|---|
| alice signs in | 44 ms (428 ms the first time, Keycloak warming); one Keycloak page, its sign-in form; a DPoP token of 52 characters, scope `cli mcp`, ending in 24 hours; no refresh token |
| The token with its proof, on the WebSocket | Welcomed as alice; `whoami` named the grant and its end |
| `gate.pass` with a `cli` token | Passed |
| No token | 401 "sign in first: run aries login" |
| The token as a bearer token, with no proof | 401 "this token is bound to a key: present it with a DPoP proof" |
| The token with a proof by another key (copied to another machine) | 401 "the DPoP proof is signed by another key than the token is bound to" |
| The same proof a second time (a replay) | 401 "dpop proof replayed: jti already seen" |
| A made-up token | 401 "the token is unknown or was revoked" |
| The `cli` token at `/mcp` (another resource) | 401 |
| Revoking the token with a connection open | Closed 1 ms after the revocation, with a close frame `{"c":"auth","m":"sign-in ended: revoked","fix":"run aries login"}`; the token then refused |
| bob, who has an account at Keycloak and no invitation | The server's callback answered 403 "no invitation for this account" |
| On `:8444`, keys from a file, a 15-second sign-in: a connection held past the end | Closed 5 ms after the grant's end (15 ms and 6 ms in runs 3 and 2), with `{"c":"auth","m":"sign-in ended at …","fix":"run aries login"}`; the token then refused, "the token expired" |

The server on `:8444` read Keycloak's key set from the file the `jwks` step wrote and made no request for keys; its sign-in still called Keycloak's token endpoint, inside the network, for the code exchange.

## 5. The remote MCP endpoint

`spike mcp` is the Go SDK's MCP client (v1.8.0) with its `AuthorizationCodeHandler`, configured for dynamic client registration and a loopback redirect, and the stand-in browser as its "open this URL".

| Step or check | Result |
|---|---|
| The flow: a 401 naming the protected resource metadata, the authorization server's metadata, registration, PKCE S256 with `resource`, Keycloak's page, the token, a tool call | 48 ms; client `dcr-…` registered; `resource` `https://aries.internal:8443/mcp`; `whoami` answered alice, scope `mcp` |
| Two calls, stateless (MCP 2026-07-28, no session header) | The same derived session, `mcp-` and eight hex digits |
| The connector's token | Bearer, an hour, with a refresh token |
| No token | 401, `WWW-Authenticate: Bearer resource_metadata="https://aries.internal:8443/.well-known/oauth-protected-resource/mcp", scope="mcp"` |
| `Origin: https://evil.example.com` | 403 |
| `Origin: https://claude.ai`, a hand-made `tools/list` | Past the origin and token checks; the SDK then refused it with `-32602` "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities", as 2026-07-28 requires |
| The connector's token at `/v1/connect` | 401 "the token is for another resource" |

**A finding: the Go SDK's client refused the authorization server on the internal network** ("non-public ip address") until the spike gave it a dialer of its own. Its documentation calls this its SSRF guard and says a custom dialer opts out. For Aries it means: an MCP client built on the SDK must be configured to reach a server on a private address; whether Claude Code applies a guard like it is not established.

## 6. A service principal

Keycloak's client-credentials token for `ci-example`, with an audience mapper naming the server and hard-coded claims in GitHub Actions' shape (`repository_owner_id`, `repository_id`, `ref`, `run_id`), was exchanged at `/oauth/token` by RFC 8693, with a DPoP proof by a new key.

| Check | Result |
|---|---|
| The exchange | A DPoP token, scope `ci`, an hour; the rule `ci-example-main` matched |
| The job connects | As `ci-example` |
| `gate.pass` with the `ci` token | Refused: `forbidden`, "passing a gate needs aries in a terminal (scope cli)" |
| The same proof at the token endpoint again | 400 `invalid_dpop_proof`, replayed |

## 7. Smart cards, Entra at another authority, and keys from a file

- **Client certificates** standing in for PIV cards, from the spike's CA, presented in the TLS handshake on `/v1/connect` with no token: alice's was welcomed as alice; mallory's, revoked in the CRL, was refused, "this certificate is revoked". No software token (SoftHSM) was used; the certificates were files.
- **Entra at a non-commercial authority** (`provider` tests): a stand-in serving Entra-shaped v2.0 discovery, keys and tokens (`tid`, `oid`, `ver`, `tenant_region_scope: USGov`) at an `https` authority of its own, trusted through a CA file. Verified, subject `oid`; refused: a token naming `login.microsoftonline.com`, another tenant, `ver` 1.0, no `oid`, another audience; with a key set file and the stand-in shut down, verified with no fetch; refused at start: no authority, no tenant, no token version, version 1 with no issuer, plain `http`, a key set file without endpoints; and a test fails if `provider.go` names a provider's host.
- **The verifier from a key set file** (`spike/identity/oidc`, `NewStatic`): a token of a key in the file verified; one of a key not in it refused naming the file; zero fetches.

## 8. Trying the spike as a connector in Claude Desktop

A custom connector in Claude Desktop (and claude.ai) is reached **from Anthropic's cloud, not from the Mac** (Anthropic's [custom connectors article](https://support.claude.com/en/articles/11175166-getting-started-with-custom-connectors-using-remote-mcp)), from `160.79.104.0/21` ([authentication for connectors](https://claude.com/docs/connectors/building/authentication)). So the spike on a laptop or an internal network cannot be added; it needs a public HTTPS address with a certificate from a public CA. Keycloak needs only be reachable by the maintainer's browser and by the spike server, never by Anthropic, since the spike is the authorization server Claude talks to.

**What the maintainer needs:** a small Linux VM with a public address, Docker and Go; a DNS name for it in a domain the maintainer controls (here `aries-spike.example.com`); ports 443 open to the internet and 8443 open to the maintainer's own address only.

1. **A certificate.** On the VM, get one for the name from a public CA with any ACME client (for example `certbot certonly --standalone -d aries-spike.example.com`). Note the paths of the full chain and the key.
2. **Keycloak**, on the VM, with that certificate, reachable at `https://aries-spike.example.com:8443`:
   `docker run -d --name kc -p 8443:8443 -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD -v /etc/letsencrypt:/le:ro quay.io/keycloak/keycloak@sha256:79c623a22abacf3fe6c00a5fb30f5b7843d717b234033b80995891c06116d244 start-dev --http-enabled=false --https-port=8443 --https-certificate-file=/le/live/aries-spike.example.com/fullchain.pem --https-certificate-key-file=/le/live/aries-spike.example.com/privkey.pem --hostname=https://aries-spike.example.com:8443`, with the password exported in the environment by name and written to a 0600 file (Keycloak's container must be able to read the key; copy it with mode 0644 to a directory only root reads, as `run.sh` does).
3. **The realm**, from this directory on the VM: `go build -o spike ./cmd/spike`, then `./spike setup -kc https://aries-spike.example.com:8443 -ca /etc/ssl/certs/ca-certificates.crt -admin-pass-file ADMINPASS -state STATE -aries https://aries-spike.example.com`. alice's password is in `STATE/alice.pass`; the server's client secret in `STATE/aries-server.secret`.
4. **The server** on 443 (as root, or with `setcap cap_net_bind_service+ep spike`): `./spike serve -addr :443 -base https://aries-spike.example.com -cert …/fullchain.pem -key …/privkey.pem -ca /etc/ssl/certs/ca-certificates.crt -issuer https://aries-spike.example.com:8443/realms/aries-spike -client-secret-file STATE/aries-server.secret`. Leave its log in view.
5. **In Claude Desktop:** Customize → Connectors → **+ Add** → **Add custom connector** (on a Team or Enterprise plan an Owner adds it under Organization settings → Connectors, and members then connect). Name `Aries spike`; URL `https://aries-spike.example.com/mcp`; Continue. Authentication **Sign in now**; OAuth client **Register automatically** (the spike does not advertise Client ID Metadata Documents, so "Use Claude's published identity" falls back to the same); Add.
6. **Connect**: Claude opens a browser at Keycloak's page. Sign in as `alice` with the password from step 3. The browser returns to Claude, and the connector shows as connected.
7. **Use it**: in a new chat, enable the connector under **+** → Connectors, and ask: "Call the whoami tool of Aries spike and show me what it returns."

**What to observe**, and send back (no token is ever printed):

- In the server's log: `register: client dcr-… ("…"), 1 redirects` with the redirect `https://claude.ai/api/mcp/auth_callback`; `callback: alice signed in … for dcr-…`; then for each request `mcp: POST /mcp from …; protocol "…"; method "…"; agent "…"; origin "…"; authorization true; dpop false`. Note the protocol version Claude sends, its user agent, whether any origin is sent, the source addresses (expected in `160.79.104.0/21`), and whether `dpop` is ever true.
- In Claude: `whoami` returns `principal: alice`, `scopes: ["mcp"]`, a session `mcp-…`, and an end a day ahead.
- After an hour: a `refresh: grant …` line, Claude's refresh of the hour's token, with no sign-in shown.
- Whether Claude shows anything when the token is refused: stop and restart the server (its state is in memory, so every token is then unknown), call the tool again, and note what Claude shows and whether it asks to connect again.
- Afterwards: remove the connector in Claude, stop both containers and the server, delete the VM and the DNS record, and delete `STATE`.

**Claude Code** can be tried without a public address, from the Mac, against the same VM or a local run: `claude mcp add --transport http aries-spike https://aries-spike.example.com/mcp`, then `/mcp` in a session to sign in. Against a server whose certificate is from a private CA, Claude Code (a Node program) needs the CA in `NODE_EXTRA_CA_CERTS`, which is Node's variable (general knowledge, not read in Claude Code's documentation).

## 9. Not tested here

- ariesd and `aries` themselves: `spike serve` stands in for the listener, the authorization server and the daemon's session; nothing of `internal/server` ran.
- Claude Desktop, claude.ai and Claude Code against the spike (section 8 is the maintainer's).
- AD FS, Entra in any cloud, Okta, Google and GitHub; Keycloak federating to Active Directory. N0.
- A real smart card, through the provider or through ariesd; SoftHSM was not used.
- Corporate TLS-inspecting proxies, cloud load balancers, and nginx with HTTP/2 to the daemon.
- Load: one client, sequential requests; no flood of unauthenticated requests.
- ACME, internal or public.
- The device grant, the URL path with a pasted address, and CIMD: designed, not run here (the first two ran in spike/identity against Keycloak's own endpoints).
