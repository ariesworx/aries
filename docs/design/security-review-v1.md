# Security review of v0.5.0: the HTTPS listener and the key proof

An adversarial review of what v0.5.0 relies on to let anyone in: the
HTTPS listener of slice T1, the key proof that authenticates over it
(`internal/keyproof`, decision 89), the WebSocket under it
(`internal/wsconn`), the proxy mode, S1's expiring keys, SSH off by
default, the listener's limits under a flood, and secrets in files,
argv, logs and spans. Those are items 2 (its key half), 3, 6, 7, 8 and
10 of [transport.md §12.4](transport.md#124-what-a-security-review-must-look-at-before-10).
It was made in parts on 16 and 17 Oct 2026, against `main` at `e482b4b`
and then `4c6d49f`, and gathered here on 17 Oct 2026, when every fix
it names had merged (`main` at `28aebc0`).

The file is named for v1.0.0, which this review was planned for. Since
decision 93 the releases before v1.0.0 are minor versions, and the
review is v0.5.0's; v1.0.0 will be reviewed again for what it adds.

Each part was tracked in Aries as a child of `aries-lx5ysigq`, and its
issue holds the full notes, with the numbers and commands behind them:

| Part | §12.4 | Issue |
|---|---|---|
| The key proof on every path, and scopes at dispatch | 2, key half | `aries-ynsuk5z4` |
| `internal/wsconn`, its fuzzing, and the hand-off into `Server.handle` | 3 | `aries-d5zg4vrh`, fuzzing `aries-ia6uatfy` |
| The listener under an unauthenticated flood | 6 | `aries-6ermswit`, flood client `aries-wnohcrfo` |
| Behind a proxy, nothing trusted from `Host` or `X-Forwarded-*` | 7 | `aries-b2ao6rsp` |
| Secrets in files, argv, logs and spans (rule 2) | 8 | `aries-afomcff2`, inventory `aries-e736bnmf` |
| SSH off by default | 10 | `aries-yvjnuedj`, `aries-xbzpwrny` |

Unlike [stage 3a's review](security-review-stage-3a.md), this one
fixed what it found as it went, each fix in a pull request of its own
with a test that failed before it; those tests are ordinary tests that
always run. The pull requests that only added tests to pin what was
found sound are named under [Looked at and found sound](#looked-at-and-found-sound).

## Summary

The core is sound. ariesd reads no `Host`, `X-Forwarded-*`,
`Forwarded` or `X-Real-IP` header anywhere: proofs, redirects and the
authorization server's metadata come from `public_urls`, and every
address is the connection's own. The principal of an HTTPS connection
comes from the key proof alone, never from what the client says, and
every op is checked against its scopes at dispatch. Nothing secret
reaches argv, a log line, a span, an exported record or the output. A
flood of unauthenticated requests beside the soak, up to 400 attacks a
second, left every check of the soak passing. A default install
exposes no SSH path.

What was found is at the edges: holds an unauthenticated client could
keep on the listener longer or larger than meant, conditions of a
hand-kept `authorized_keys` line that HTTPS did not honor, files read
whatever their mode, and what the proof is and is not bound to.
Twenty-one findings, one high:

| | Severity | Finding | Proof | Disposition |
|---|---|---|---|---|
| L-1 | high | A request that names a body and sends none holds its connection for ever, on any path | demonstrated | fixed, #229 |
| L-2 | medium | A kept-alive connection idles 2 minutes after one answer | demonstrated | fixed, #230 |
| L-3 | medium | One held request adds 5 seconds to every stop and restart | demonstrated | deferred, `aries-hguptcq2` (v0.6.0) |
| L-4 | medium | The per-address limits count each IPv6 address alone, so a /64 has none | read | fixed, #227 |
| L-5 | low | Nothing bounds the rate of new connections or TLS handshakes from one address | measured | deferred, `aries-bt467whn` (v0.6.0) |
| K-1 | medium | Over HTTPS, a hand-kept line's `from=`, `expiry-time`, `cert-authority` and `verify-required` are ignored | demonstrated | fixed, #221 |
| K-2 | medium | The key proof is not bound to the TLS session, so a holder of a certificate the client trusts can relay a proof | read | accepted for v0.5.0 and documented; fixed for v0.6.0 where ariesd terminates TLS (protocol 9), `aries-tl2cc2df` |
| K-3 | low | An ended key's refusal names its principal and end to anyone holding the public key, before any proof | demonstrated | fixed, #223 |
| W-1 | medium | Before its proof, a WebSocket could make the server allocate 4 MiB per connection | demonstrated | fixed, #234 |
| W-2 | low | After its proof, a connection's writes have no deadline | read | deferred, `aries-gl5dhcr5` (v0.6.0) |
| P-1 | medium | Behind a proxy no limit per address applies, and the documented nginx block set none | demonstrated | fixed, #232 |
| P-2 | low | Behind a proxy, one client's refusals use up every client's refusal-log allowance | read | deferred, `aries-uxkbuv7r` (v0.6.0) |
| P-3 | info | `trusted:` took no IPv4 network written in IPv6's form (fails closed) | demonstrated | fixed, #218 |
| S-1 | medium | ariesd uses a Postgres passfile or client key others can read | demonstrated | fixed, #224 |
| S-2 | low | `aries` signs with a private key file others can read | demonstrated | fixed, #225 |
| S-3 | low | `client_secret_file` is checked by name, then read by name | read | deferred, `aries-szcyh6ug` (v0.6.0) |
| S-4 | info | Files inside the ACME cache are not mode-checked on read | read | accepted |
| S-5 | info | A YAML type error quotes up to 7 characters of a value | demonstrated | accepted |
| H-1 | medium | With SSH off, ariesd's admin commands ran unconfined by AppArmor | read | fixed, #193 |
| H-2 | low | The Docker test server published the SSH port with SSH off | demonstrated | fixed, #198 |
| J-1, J-2 | – | `/oauth/revoke` answers 200 with no token; a wrong method gets 404 | read | accepted |

Each part's notes number its own findings (the flood review's F-1 is
L-1 here); the [Disposition](#disposition) table gives both.

## Demonstrated

### L-1 (high): a body named and never sent holds the connection

**Adversary.** Anyone who can reach the port.

**Where.** `internal/server/https.go`, `serveHTTPS`: `ReadHeaderTimeout`
ended with the headers and no `ReadTimeout` was set, and `net/http`
reads a body its handler left unread (up to 256 KiB) before it writes
the answer.

**Reproduce.** Any path or method, a 404 or `GET /.well-known/aries`
among them, with `Content-Length: 1000` and no body, was neither
answered nor closed; with a byte every 100 ms it stayed open past 4
seconds at a 200 ms header timeout. In the flood soak at 100 attacks a
second, 1,893 of 2,694 such connections were still held at 30 seconds.

**What is gained.** 64 such connections from each of 64 addresses take
all 4,096 places of `https_conns_per_server`, and every session is shut
out, with no credential.

**Fix.** #229 (`aries-xkbtokxk`): a read deadline of
`https_body_timeout` (10 s) once the headers are in. Under the same
flood, none was held and every attack closed within 15 seconds.

### L-2 (medium): an idle kept-alive connection held its place 2 minutes

**Where.** `IdleTimeout: 2 * time.Minute`, fixed in the code. Once L-1
is fixed, it is the cheapest hold: one request per place every 2
minutes, against a new connection every 10 to 20 seconds for any other.

**Fix.** #230 (`aries-kwkvbcyu`): the idle time is
`https_header_timeout`. Its test failed first: the connection was still
open 3 seconds later at a 100 ms timeout.

### L-3 (medium): a held request delays every stop by 5 seconds

**Where.** `serveHTTPS` stops with `srv.Shutdown` under `shutdownWait`
(5 s), which waits for every plain request in flight, and a slow body
or slow headers is always one.

**Reproduce.** Serve returned at once with nothing held, and after
5.001 seconds with one request held. In 90-second soaks with two
restarts each, session handshakes refused `unavailable` went from 53
without a flood to 4,453 with a flood of 20 a second; every check still
held.

**What is gained.** Anyone who keeps one slow request open turns each
restart or `ariesd upgrade` into a 5-second outage. Nothing is read or
written.

**Disposition.** Deferred to v0.6.0, `aries-hguptcq2`: at shutdown,
close at once a request that has proved nothing. Accepted for v0.5.0
because it costs availability for 5 seconds at a time the operator
chose, and only while the operator restarts.

### K-1 (medium): hand-kept line conditions ignored over HTTPS

**Where.** `internal/server/keys.go`, `bridgePrincipal`, read by
`keyOwner` through `handBridgeOwner`. The key proof takes a key from a
line of the operator's `authorized_keys` that forces the bridge, "as
sshd would take it", but read only its `command=`. sshd also holds the
key to `from=`, `expiry-time`, `cert-authority` (a CA's key, which
signs in as no one) and `verify-required`.

**Reproduce.** `TestHTTPSHandLineConditions` (`internal/server`)
welcomed all five such lines before the fix.

**What is gained.** An operator limits a contractor's line to
`from="10.0.0.0/8"`, or gives it an `expiry-time`; HTTPS, on by
default, took the key from anywhere and after its end.

**Fix.** #221 (`aries-n2kzuhhn`): a line with any of the four is no
one's over HTTPS, refused as an unknown key. It fails closed: behind a
proxy the daemon cannot see the address `from=` names, and the patterns
are sshd's. Over SSH nothing changes. The guides say so and point at
`--expires`.

### K-3 (low): an ended key named before any proof

**Where.** `web.prove` (`https.go`) through `endedOffer` (`expiry.go`):
an offer whose only known key had ended was refused at once, the fix
naming the principal and the message the end, and the refusal was not
charged to the address.

**Reproduce.** `TestHTTPSKeyEndedUnproved` (`internal/server`) got the
refusal naming carol before the fix.

**What is gained.** Forges publish public keys, so anyone could map one
to its Aries principal and the date its access ended.

**Fix.** #223 (`aries-bfdpgsyh`): the ended key is challenged, as a
disabled principal's is, and told it ended once proved; another key's
proof is `bad_proof`, and charged.

### W-1 (medium): 4 MiB held per unauthenticated WebSocket

**Where.** Before its key was proved, a WebSocket's messages were
bounded by `ws_message_bytes` (4 MiB), and `readFrame` allocated a
frame's declared length before reading it.

**Reproduce.** `TestReadAllocatesWhatArrives` (`internal/wsconn`)
measured 4,199,656 bytes allocated for a 4 MiB header followed by 16
bytes; `TestHTTPSProofMessageBound` (`internal/server`) read and
answered a 65,537-byte message before any proof.

**What is gained.** An unauthenticated peer upgrades, declares 4 MiB
and drips bytes for `upgrade_timeout` (10 s): 4 MiB held per
connection, up to `https_conns_per_server` of them, 16 GiB at the
default.

**Fix.** #234: `readFrame` grows the payload in 64 KiB chunks as bytes
arrive, and `prove` holds a connection to the proof's bound (64 KiB,
`wsconn.Conn.SetMax`) until its key is proved, restoring
`ws_message_bytes` after. Both tests failed before and pass after.
`FuzzRead` ran a minute (14.3 million executions) on the new read path
with no finding.

### P-1 (medium): no per-address limit behind a proxy

**Where.** Behind a proxy that terminates TLS every connection is the
proxy's, so `https_conns_per_addr`, `auth_failures_per_minute` and
`signins_pending_per_addr` do not apply, by design (ariesd trusts no
`X-Forwarded-For`). docs/server.md said to keep the proxy's own, but its
nginx block and `deploy/docker/nginx.conf` set none.

**Reproduce.** Through the Docker test server's `proxy` profile, one
client sent 12 refused key proofs in a row, none slowed.

**What is gained.** One client behind the proxy can hold every place of
`https_conns_per_server`, each until `upgrade_timeout`.

**Fix.** #232 (`aries-keza5qm7`): `limit_conn` and `limit_req` by
client address in the documented block and the fixture, which say they
stand in for ariesd's own; `scripts/nginx_test.go` holds the two alike.

### P-3 (info): an IPv4 network in IPv6's form trusted no proxy

**Where.** `settings.go`, `Trusted`, unmapped a single `::ffff:`
address but not a `::ffff:…/104` network, while `peerAddr` unmaps
every peer. The operator's own proxy was refused as untrusted: it
failed closed.

**Fix.** #218 (`aries-rm4qakao`): a 4-in-6 prefix of 96 bits or more is
taken as IPv4's.

### S-1 (medium): a Postgres passfile or client key others can read

**Where.** pgx reads `passfile=` (or `PGPASSFILE`, or `~/.pgpass`) and
`sslkey=` (or `PGSSLKEY`, or `~/.postgresql/postgresql.key`) with no
mode check, where libpq refuses a loose one.

**Reproduce.** `TestDatabaseSecretFilesRefuseLooseModes`
(`internal/server`) failed in 12 of 12 cases.

**What is gained.** `install-server.sh --db-password-file` writes the
passfile 0600, and the guide puts the password there. A restore, a hand
edit or a configuration-management run leaves it 0644; ariesd started
without a word, and every local user could read the password of a role
that reaches all of the server's data.

**Fix.** #224 (`aries-l6emugw5`): ariesd refuses such a file at start,
naming `chmod 600`, as it refuses a config file holding a password.

### S-2 (low): `aries` signs with a key file others can read

**Where.** `internal/client/conn.go`, `readKeyFile` (`key:`,
`ARIES_KEY_FILE`) and `readDefaultIdentity` (`~/.ssh`) signed with an
unencrypted key whatever its mode, where OpenSSH refuses one.

**Reproduce.** `TestLooseKeyFileRefused` (`internal/client`) failed in
4 of 6 cases.

**Fix.** #225 (`aries-rpp7gz6g`): a configured loose key is refused
with `chmod 600`, and a loose default identity is passed over with that
reason.

### S-5 (info): a YAML type error quotes part of a value

A config file with a value of the wrong type gets yaml.v3's error,
which quotes up to 7 characters. A DSN password never reached it in the
probe; it would take a short password alone under a typed key.
Accepted: the error goes to the operator who wrote the file, on the
terminal or journal that already reads it.

### H-2 (low): the Docker test server published port 22 with SSH off

`compose.yaml` published `127.0.0.1:2222:22` whatever `ARIES_SSH` was,
so a default stack, which serves no SSH, still held the port. Fixed in
#198 (`aries-xbzpwrny`): the mapping moved to `compose.ssh.yaml`, added
only with `ARIES_SSH=1`. A test fixture, never production; it is listed
because §12.4 item 10 asks that a default exposes no SSH path.

## Read from the code, not demonstrated

### L-4 (medium): IPv6 addresses counted alone

`peerAddr` keyed `https_conns_per_addr`, the auth-failure bucket and
`signins_pending_per_addr` by the whole address, and an IPv6 client is
given at least a /64, so none of the three bounded it; and because
`signins_pending_per_server` lets go of the oldest when full, one /64
could push out everyone's pending sign-ins. Fixed in #227
(`aries-2gpal65n`): `https_addr_prefix_v6` (64) names a client by its
leading bits. `TestListenerCountsAnIPv6NetworkOnce` and
`TestPendingSignInsShareAnIPv6Network` hold it.

### L-5 (low): no rate on new connections

§4.6 caps the connections an address holds, not how fast it opens them,
and junk that names no key (a silent or malformed upgrade, a bad
`/oauth/*` request) takes nothing from `auth_failures_per_minute`. At
400 attacks a second from one address, without `-race`, the soak's
sessions fell from 995 to 572 requests a second and their p99s rose two
to three times, every check passing; the flood's own TLS clients shared
the process, so the daemon's own cost is lower. Deferred to v0.6.0,
`aries-bt467whn`: a bucket of new connections per address, and a
malformed upgrade charged to the address. Accepted for v0.5.0: it
slows sessions and refuses none, and a default must be weighed against
offices behind one NAT address.

### K-2 (medium): the key proof is not bound to the TLS session

**Where.** The transcript of `internal/keyproof` signs a domain, the
server's URL, the connection's nonce and the key, and nothing of the TLS
session.

**Scenario.** Whoever holds a certificate the client trusts for the
server's name (one issued in error, a CA in `server.ca` whose key
leaked, a TLS-inspecting middlebox) terminates the client's TLS, opens
its own to ariesd, relays the offer, the challenge and the proof, and is
that principal on the real server for the connection, an admin's
rights included. Over SSH the same attacker gets nothing: publickey
authentication signs the session id, and the host key is pinned.

**Disposition.** Accepted for v0.5.0, with the certificate check as the
defense, which [docs/security-model.md](../security-model.md#the-https-listener)
states with this case (#202), telling operators to keep `server.ca` to
the CA that issues the server's certificate. Deferred to v0.6.0,
`aries-tl2cc2df`: where ariesd terminates TLS itself, the challenge asks
for RFC 9266's `tls-exporter` and the client signs a transcript that
adds it, so that a relay's two sessions differ and the proof fails.
Behind a proxy it cannot apply, since ariesd never sees the client's
session, and the proxy's certificate stays the only defense. The
review recommended it before v0.5.0, when it would cost nothing in
compatibility; after v0.5.0 it is a protocol change under rule 10, and
old clients need a transition in which the binding is optional.

**Fixed for v0.6.0** (`aries-tl2cc2df`, protocol 9). Where ariesd
terminates TLS, the challenge asks for the `tls-exporter` and a client
of 9 signs a v2 transcript bound to its session; `TestHTTPSProofRelayed`
(`internal/server`) relays a proof between two TLS sessions and holds
its refusal, whose fix names TLS inspection. A client of 9 never signs
the v1 transcript, so a relay that strips or changes the binding gains
nothing from it. The transition: the server still takes v1 from
v0.5.0's clients, which a relay can still take, until `ProtoMin` moves
to 9, or until an operator sets `https: refuse_key_proof_v1`. Behind a
proxy nothing changes. [transport.md, As built 17 Oct 2026](transport.md#as-built-17-oct-2026-the-key-proof-bound-to-the-tls-session)
has the design.

### W-2 (low): no write deadline after the proof

After the proof, `prove` clears both deadlines and nothing sets a write
deadline again. A signed-in client that pings and stops reading blocks
the pong's write in `Read`'s goroutine, holding the write lock, so the
connection's reads, pings and pushes stop and its place stays held until
the kernel gives up. Deferred to v0.6.0, `aries-gl5dhcr5`. Accepted for
v0.5.0: it needs a key the server knows, and `conns_per_principal`
bounds what one principal holds.

### P-2 (low): one client's refusals hide everyone's in the log

The listener's refusals are logged sampled per address, and
behind a proxy the address is always the proxy's. A client looping
refused proofs uses up the allowance, and for the rest of the minute
others' refusals appear only in the next line's suppressed count; the
metrics still count every one, and no field is the attacker's to
choose. Deferred to v0.6.0, `aries-uxkbuv7r`. Accepted for v0.5.0: the
proxy's access log is the record of each client, as docs/server.md
says.

### S-3 (low): `client_secret_file` checked, then read, by name

`internal/oidc/provider.go`, `readSecret`, calls `os.Stat` and then
`os.ReadFile` on the same path, so a path swapped between the two is
read with a mode nobody checked. Deferred to v0.6.0, `aries-szcyh6ug`:
open once and check the open file. Accepted for v0.5.0: the path is the
operator's, in a directory the operator keeps, and providers ship off
unless configured. Otherwise the secret is clean: it is read only from
that file, sent only in the token request's body, and never in an error.

### S-4 (info): files inside the ACME cache are not checked on read

autocert writes its files 0600 through a temporary file and a rename,
and ariesd refuses at start a cache directory others can enter. A file
loosened later inside it is not refused, but the 0700 directory still
closes it to others. Accepted; no change.

### H-1 (medium): admin commands unconfined with SSH off

AppArmor chooses a profile by a program's path. With SSH on, the
binary's path carries the bridge's profile, `ariesd-stdio`; with SSH
off, the default, the installer does not load it, so `ariesd status`
and an operator's command run without `aa-exec -p ariesd-admin` ran
unconfined. Fixed in #193 (`aries-yvjnuedj`): with SSH off the path
carries `ariesd-default`, the operator's rules, shared with
`ariesd-admin` through `abstractions/ariesd-admin`. CI parses the
profiles; loading them in enforce mode on a machine is P5's (below).

### J-1 and J-2: judged and kept

- **J-1.** `POST /oauth/revoke` answers 200 with no token or an unknown
  one. RFC 7009 §2.2 asks 200 for an invalid token; known and unknown
  get the same answer, so it is no oracle, and anyone holding a token's
  string may end it, which is what revocation is for. Accepted; tokens
  are reviewed in 1.x.
- **J-2.** A wrong method on a listed path gets 404 from the catch-all,
  not 405. Every unlisted request is answered alike, it reveals nothing
  that `/.well-known` does not, and `aries` never sends one. Accepted.

`TestHTTPSRoutes` (#196) pins both.

## Could not determine

- **`https_conns_per_addr` at its default under a flood from its own
  address.** The flood ran on macOS, where no `127.0.0.2` could be made
  without root, so the run raised the cap to 4,096 and only the server's
  cap and the timeouts were exercised. A soak on Linux with
  `ARIES_SOAK_FLOOD_FROM=127.0.0.2` would show it; `soak.yml` has no
  flood row.
- **Exhausting `https_conns_per_server`.** At 1,000 attacks a second the
  machine ran out of ephemeral ports first.
- **The daemon's own CPU per TLS handshake**, apart from the flood's
  clients in the same process (L-5).
- **The dpop fuzz target's execution count**: its run logged to the
  file the oidc run then overwrote. It completed its hour, exit 0, with
  no finding.

## Looked at and found sound

- **Every path the listener serves**, each with its check. `GET` and
  `HEAD /.well-known/aries`: no check, and no principal in the answer.
  `/.well-known/oauth-authorization-server`, `/oauth/*` and
  `/auth/callback`: only with `login: providers`, and out of scope (Not
  reviewed). `GET /v1/connect`: an `Origin` refused 403; under direct
  TLS the address's auth-failure budget (429); with `Authorization`, the
  token and DPoP before the upgrade; without, the key proof within
  `upgrade_timeout`. Everything else, a wrong method included, 404; an
  unclean path, a 307 to its clean path on this server, never `//host`.
  Every answer carries HSTS, `nosniff` and `no-store`, its body capped.
  `TestHTTPSRoutes` (#196, extended in #220).
- **The key proof** (#220). The URL is checked against `public_urls`,
  never `Host` or `X-Forwarded-*` (`TestHTTPSForwardedHeadersIgnored`); a
  fresh 32-byte nonce, so a proof captured on one connection is refused
  on the next (`TestHTTPSProofReplay`); the challenge names the first
  known key, and another offered key's proof is refused
  (`TestHTTPSChallengesKnownKey`); after the proof only a hello is read
  (`TestHTTPSNothingButHelloAfterProof`); the transcript's domain cannot
  be an SSH session id's or SSHSIG's, and its parts are length-framed
  (`TestTranscriptFraming`); a signature's format must be its key's, RSA
  with SHA-2 only (`TestVerifyAlgorithms`); escaped paths, an empty query
  or fragment and a backslash in the host are refused
  (`TestNormalizeURLRefuses`).
- **S1's expiring keys over HTTPS.** An ended key proves nothing, and an
  open connection closes at its key's end (`TestHTTPSKeyEnded`); the
  handshake checks again, so a key that ends between proof and hello is
  refused.
- **Scopes at dispatch.** `mayCall` runs before every op, admin ops and
  `watch` included. `TestScopesAtDispatch` walks every op of
  `internal/proto` over real connections of scope `cli`, `mcp` and `ci`,
  and was seen to fail when dispatch skipped `mayCall` for one op.
  `TestEveryOpHasScopes` was seen to fail for an op added without
  scopes, and `TestEveryOpHasScopesFails` keeps it so (#220).
- **The hand-off into `Server.handle`.** The principal and the key's
  fingerprint come from the bridge frame `prove` builds, never from the
  client's hello; the session and machine are the client's, as rule 12
  intends.
- **`internal/wsconn`'s framing.** Reserved bits, unknown opcodes,
  fragmented or oversized control frames, lengths not in shortest form
  or past 2^63, wrong masking for the side, text that is not UTF-8 and
  close codes no endpoint may send each close the connection with the
  code that says why, and the error sticks; at most 64 control frames
  inside a fragmented message; HTTP/1.1 `GET` only, version 13, a
  16-byte key; `Close` never waits on a write stuck behind a peer that
  reads nothing.
- **Fuzzing** (`aries-ia6uatfy`), an hour each with no crash:
  `FuzzRead` (`internal/wsconn`, 124.9 million executions), `FuzzParse`
  (`internal/jose`, 284.1 million), `FuzzVerify` (`internal/dpop`; count
  lost, above), `FuzzVerify` (`internal/oidc`, 120.7 million),
  `FuzzParseOffer` and `FuzzParseProof` (`internal/keyproof`, 222.2 and
  213.5 million), `FuzzDecoder` (`internal/proto`, 169.0 million).
- **Nothing trusted from a proxy's headers** (#217,
  `internal/server/forged_test.go`). Forged `Host`, `X-Forwarded-*`,
  `Forwarded` and `X-Real-IP`, directly and through the Docker test
  server's nginx, change no proof, redirect, limit or log field: a proof
  for a forged host is refused with a fix naming the configured URL; the
  address charged is the connection's, so rotating `X-Forwarded-For`
  still reaches 429; no log line held a forged host or address; and in
  proxy mode an untrusted peer is closed before a byte is read.
- **The listener under a flood** (`aries-6ermswit`, with #199's flood
  client). Nine runs beside the soak, from 60 seconds to 5 minutes, at
  20, 100 and 400 attacks a second, with and without `-race`, every
  check of the soak passing as in three baselines. Each timeout of
  §4.6 did what it says: slow headers closed at 10 s, silent and junk
  upgrades at 10 s and under 5, junk `/oauth/*` answered 400, a plain
  `/v1/connect` 400. About 2.6 goroutines per held connection, so 4,096
  are affordable. Each limit was judged and kept at its default; the
  one default found too loose was the fixed idle time (L-2), and the one
  missing bound was L-1's.
- **Secrets** (#222, #201). Every file that holds a secret is written
  0600 in a 0700 directory where Aries writes it, and refused when
  looser where Aries reads it: the TLS key, the ACME cache directory, a
  config file holding a DSN password, the passfile and client key (S-1),
  the telemetry stand-in key, the client's SSH keys (S-2), the sign-in
  token and DPoP key, a provider's client secret (S-3), and the outbox
  and cache. `TestNoSecretInLogsSpansOrOutput` (`internal/e2e`: the key
  proof, an admin session, a refused key, a sign-in used, forged and
  ended) and `TestRunDaemonLeaksNoSecret` (`cmd/ariesd`: a DSN password,
  TLS files, OTLP with a credential, the stand-in key, a refused start)
  grep the daemon's log, spans, exported records, raw OTLP, stdout,
  stderr and refusal lines for every secret value, and fail when one is
  planted. A password on `--dsn` is refused; no other secret has a flag.
- **SSH off by default.** `transports:` defaults to `[https]`; ariesd
  then writes no keys file, removes one it wrote, and refuses a bridge
  frame from `ariesd stdio` (`unavailable`). Without `--enable-ssh` the
  installer writes nothing sshd reads for the `aries` account, and
  `--plan` and a test hold each profile to what it writes
  ([SSH on and off](../server.md#ssh-on-and-off)).

## Not reviewed

- **Sign-in at an identity provider, Aries's tokens, DPoP and the
  workload token exchange** (slices T2 and N5; §12.4 items 1, 4, 5 and
  9, and the token half of 2). They are in v0.5.0's build but off unless
  an operator configures `login: providers` or `workload: providers`,
  and v0.5.0 does not promise them (decision 91). Their review, with
  sign-in against a real provider, is 1.x's: `aries-sqjvkqdz`. Their
  fuzz targets (`internal/jose`, `internal/dpop`, `internal/oidc`) ran
  above, and their secret files were checked, but nothing here judges
  the authorization server's flows.
- **Channel binding**, K-2's fix: v0.6.0's, `aries-tl2cc2df`.
- **ACME against a real CA.** TLS by ACME has run only against a test
  CA; a run against Let's Encrypt's staging and production, from a host
  with a public name, is v0.6.0's, `aries-pk55wedb`.
- **The AppArmor profiles enforced on a machine**, and the soaks and the
  installer on Linux and amd64: P5's machine workflow. CI parses the
  profiles; it does not run ariesd confined.
- The remote MCP endpoint, which is not built (T3); SSH itself, which
  is sshd's and was reviewed in earlier stages; the Docker test server
  beyond its port and its proxy; Windows.

## Disposition

| | Was | Disposition | Where |
|---|---|---|---|
| L-1 | flood F-1 | Fixed: `https_body_timeout` | #229, `aries-xkbtokxk` |
| L-2 | flood F-2 | Fixed: idle time is `https_header_timeout` | #230, `aries-kwkvbcyu` |
| L-3 | flood F-3 | Deferred to v0.6.0 | `aries-hguptcq2` |
| L-4 | flood F-4 | Fixed: `https_addr_prefix_v6` | #227, `aries-2gpal65n` |
| L-5 | flood F-5 | Deferred to v0.6.0 | `aries-bt467whn` |
| K-1 | key proof F-1 | Fixed: a conditioned hand-kept line is no one's over HTTPS | #221, `aries-n2kzuhhn` |
| K-2 | key proof F-2 | Accepted for v0.5.0, documented in security-model.md (#202); fixed for v0.6.0 where ariesd terminates TLS, with v0.5.0's clients served until `ProtoMin` is 9 | `aries-tl2cc2df` |
| K-3 | key proof F-3 | Fixed: an ended key is told so only once proved | #223, `aries-bfdpgsyh` |
| W-1 | wsconn WS-1 | Fixed: chunked reads, the proof's 64 KiB bound | #234 |
| W-2 | wsconn WS-2 | Deferred to v0.6.0 | `aries-gl5dhcr5` |
| P-1 | proxy PX-2 | Fixed: per-address limits in the nginx block and the fixture | #232, `aries-keza5qm7` |
| P-2 | proxy PX-3 | Deferred to v0.6.0 | `aries-uxkbuv7r` |
| P-3 | proxy PX-4 | Fixed | #218, `aries-rm4qakao` |
| S-1 | secrets S-1 | Fixed: a loose passfile or client key refused | #224, `aries-l6emugw5` |
| S-2 | secrets S-2 | Fixed: a loose key file refused or passed over | #225, `aries-rpp7gz6g` |
| S-3 | secrets S-3 | Deferred to v0.6.0 | `aries-szcyh6ug` |
| S-4 | secrets S-4 | Accepted: the 0700 directory closes the files to others | – |
| S-5 | secrets S-5 | Accepted: the error reaches only the operator | – |
| H-1 | – | Fixed: the `ariesd-default` profile | #193, `aries-yvjnuedj` |
| H-2 | – | Fixed: port 22 only with `ARIES_SSH=1` | #198, `aries-xbzpwrny` |
| J-1, J-2 | key proof J-1, J-2 | Accepted, pinned by `TestHTTPSRoutes` | #196 |

The tests that pin what was found sound: #196 (routes), #199 (the flood
client), #201 (secret files' modes), #217 (forged headers), #220 (the
key proof and scopes) and #222 (secrets in logs, spans and output).

Found along the way, not security findings, and filed for v0.6.0: a
leak check of `TestDeadCollector` that can fail under load
(`aries-2zvhd5br`), and two macOS flakes in the full suite
(`aries-v2xpd77u`, `aries-w5yhbg7n`).
