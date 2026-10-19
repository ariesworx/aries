# Running Sextant

Sextant is the part of Aries that turns what the agents spend into
series a dashboard draws and an alert watches. It is a binary of its
own, `sextant`, that runs beside `ariesd` (or anywhere that can reach
it), reads the projects it is given through the same ops `aries`
sends, and exports what it reads over OTLP and on a Prometheus
endpoint. It changes nothing in Aries, never touches the database, and
reads with its principal's rights, as a person would.
[docs/design/sextant.md](design/sextant.md) is the design.

**What is built (slice X0).** The binary, its config, signing in by key
or as a service principal, the poll loop against one `ariesd`, OTLP
and Prometheus out, its own failures as `sextant.scrape.errors`, and
its limits. It exports one figure of cost: each project's list-price
cost over the window, by model. Cost by account, tokens, hours,
amortized plans and unpriced tokens (X1), the flow of issues (X2), a
dashboard and alert rules (X3) and a release of its own with an
installer line, a systemd unit and an AppArmor profile (X5) are not
built. Until X5, Sextant has no release: build it from a checkout.

## Build it

From a checkout of the repository, on the Go its `go.mod` names:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o sextant ./cmd/sextant
./sextant -version
```

It builds for Linux, macOS and Windows; it is meant to run on the
server's machine, under its own account, as `ariesd` does. `aries`
links none of it, and it links nothing `ariesd` does not
(`internal/depcheck` holds both).

## Give it a principal

Sextant reads as a member of each project it reads, with no admin
right: `cost` and `prices` are a member's ops. Give it a principal of
its own, so that what it reads is recorded as its, and nothing else
holds its key.

**By key**, over HTTPS or SSH. Make a key that only Sextant's account
can read, and add it:

```sh
ssh-keygen -t ed25519 -N '' -C sextant@aries.example.com -f /etc/sextant/id_ed25519
aries admin principal add sextant --kind service --owner alice --key /etc/sextant/id_ed25519.pub
aries admin member add sextant --project acme     # once for each project it reads
```

On a server of one project, the key's line in the server's
`authorized_keys` is the membership ([Running a server](server.md)).
A key may be given an end with `aries admin key add --expires`.

**As a service principal**, with its platform's token and no key or
secret of its own, over HTTPS, on a machine of a platform the server
trusts (Google Cloud, Azure, or a GitHub Actions job): a server admin
adds the principal, a trust rule that pins the machine's identity, and
the membership, as [Service principals](server.md#service-principals)
describes:

```sh
aries admin principal add sextant --kind service --owner alice
aries admin trust add sextant-vm --principal sextant --provider gcp \
    --claim sub=111111111111111111111 --claim email=sextant@example-project.iam.gserviceaccount.com
aries admin member add sextant --project acme
```

Sextant then signs in at the server's `/oauth/token` with the
platform's token, keeps the sign-in in its `data_dir`, presents it on
each connection, and signs in again two minutes before it ends.

## Configure it

`sextant.yaml`, mode 0600 or at least written by no one else (Sextant
refuses a config others can write: it names the key Sextant offers):

```yaml
server:                      # as in .aries.yaml: url (and ca) for HTTPS,
  url: https://aries.example.com   # or host, port, user and host_key for SSH
  # ca: ca.pem               # a file beside sextant.yaml, or the PEM itself
key: /etc/sextant/id_ed25519 # sign in by key: the only key Sextant offers
# workload:                  # or as a service principal
#   kind: google             # azure, github-actions or google
#   provider: gcp            # the server's workload provider, if it has several
# data_dir: /var/lib/sextant # a directory of Sextant's own, for the sign-in
projects:                    # the ids, as each project's .aries.yaml names them
  - 6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b
interval: 60s                # how often every project is read
window: 24h                  # how far back cost is summed
listen: 127.0.0.1:9466       # /metrics, /healthz and /readyz; off for none
limits:
  projects: 50
  models: 20
  request_timeout: 30s
```

Check it, then run it:

```sh
sextant -config /etc/sextant/sextant.yaml -check
sextant -config /etc/sextant/sextant.yaml
```

It runs until an interrupt or SIGTERM, then makes the last export and
exits 0. It exits 1 on a failure, with a line that names the fix, and
2 on a mistake on the command line. It logs to standard error, one
line when it starts, one when a project cannot be read (and every ten
minutes while it still cannot), one when it is read again, and one at
the end; never a key, a token or what the server holds.

| Key | Default | Bounds | What it is |
|---|---|---|---|
| `server` | required | as `.aries.yaml`'s | The server; `ARIES_TRANSPORT=ssh` in Sextant's environment pins SSH, as for `aries` |
| `key` | | a file | The key Sextant signs in with, absolute or relative to the config's directory. One of `key` and `workload` |
| `workload.kind`, `.provider` | | `azure`, `github-actions`, `google` | A service principal's sign-in, over HTTPS |
| `data_dir` | | required with `workload` | Where the sign-in is kept, 0700 |
| `projects` | required | 1 to `limits.projects`, each once | The projects read, one after another |
| `interval` | 60s | 10s or more | How often every project is read |
| `window` | 24h | 1m to 744h (31 days) | The window cost is summed over |
| `listen` | 127.0.0.1:9466 | a loopback address, or `off` | The listener |
| `limits.projects` | 50 | 1 to 500 | Projects listed, and values of the `project` label |
| `limits.models` | 20 | 1 to 500 | Values of the `model` label; the rest are summed into `(other)` |
| `limits.request_timeout` | 30s | 1s to 2m | A connection's start, and each request |

Each poll of a project sends `prices` and then `cost --group-by model`
for the window, with at most `limits.models` groups: what one person
running `aries cost` sends. Sextant keeps one connection to each
project open between polls and makes a new one when it is lost.

## What it exports

| Metric | Prometheus | Kind | Labels | What it is |
|---|---|---|---|---|
| `sextant.cost.usd` | `sextant_cost_usd` | gauge | `project`, `model` | The list-price cost of the window's tokens, by model, as of the last poll |
| `sextant.scrape.errors` | `sextant_scrape_errors_total` | counter | `project`, `op`, `code` | Sextant's failures to read a project |

- **A gauge, not a counter.** A cost is computed on read from prices
  that can change, so each poll reports the window's truth again. Draw
  it as it is: `increase()` over it means nothing
  ([sextant.md](design/sextant.md) §6, recommendation 2).
- **`model`** is a model the server has a price for, among the
  `limits.models` with the largest cost; any other is `(other)`. A
  model's name is text a client reported, and only the price table is
  the server's, as for ariesd's `aries.usage.tokens` (decision 92).
  Tokens with no price cost nothing at list price, so `(other)` may be
  0; `aries cost` names the unpriced models.
- **`project`** is the project's id. One past `limits.projects`, which
  the config cannot list, would be `other`.
- **`op`** is `connect` (the dial, the sign-in and the handshake),
  `prices` or `cost`, and **`code`** the error's code (`unavailable`,
  `auth`, `forbidden`, `busy` and the rest of the protocol's), or
  `other`.
- **A project that cannot be read has no cost series** until it is
  read again, so a dashboard shows a gap rather than an old figure.
  Alert on `increase(sextant_scrape_errors_total[15m]) > 0`, and on
  Sextant's own `up` in Prometheus.

**Prometheus** scrapes `http://127.0.0.1:9466/metrics`. `/healthz`
answers 200 while Sextant runs, and `/readyz` 200 once its first poll
has run, whatever it found. The listener is the one `ariesd` uses
([The telemetry listener](security-model.md#the-telemetry-listener)):
loopback only, with no authentication, at most 8 requests at once. Set
`listen: off` to push only.

**OTLP.** Sextant pushes its metrics over OTLP/HTTP when its
environment names an endpoint, with the same `OTEL_*` variables as
`ariesd` ([Observability](server.md#observability)):
`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`,
their headers and timeouts, `OTEL_METRIC_EXPORT_INTERVAL`,
`OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SDK_DISABLED`. Its
`service.name` is `sextant` unless `OTEL_SERVICE_NAME` says otherwise.
It exports no traces or logs, and none of ariesd's instruments, not
even the Go runtime's.

## What it does not do

- It keeps no history: the metrics backend does.
- It reads no issue's text, and gives no figure by person: no metric
  has a person label (sextant.md §4; decision 98).
- It sets no budget and stops no spend. A budget is an alert rule on
  its figures, and a hard stop is set at each model provider (decision
  99).
- It reads one `ariesd`. Several servers, and a hosted service, come
  after it is proved against one.
- It replaces nothing of `aries cost`, which stays exact and on demand;
  Sextant samples.
