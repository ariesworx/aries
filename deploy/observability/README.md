# Observability examples

Seven files to start from when wiring an Aries server into what you
already monitor with. **They are examples that are tested, not a
product.** [Running a server](../../docs/server.md#a-collector-alert-rules-and-a-dashboard)
says how to use each, and lists the alerts.

| File | Is |
|---|---|
| [collector.yaml](collector.yaml) | An OpenTelemetry Collector's configuration: OTLP in from ariesd, a scrape of ariesd's `/metrics`, and one exporter to replace with your backend's |
| [alerts.yaml](alerts.yaml) | Alert rules in Prometheus's rule format |
| [prometheus.yaml](prometheus.yaml) | A Prometheus configuration that scrapes ariesd and loads the rules |
| [dashboard.json](dashboard.json) | The server's dashboard, in Grafana's format |
| [dashboard-requests.json](dashboard-requests.json) | Requests by op, in Grafana's format |
| [dashboard-traces-logs.json](dashboard-traces-logs.json) | Traces and logs, for a Grafana with Tempo and Loki |
| [grafana-dashboards.yaml](grafana-dashboards.yaml) | Grafana's provisioning of the three dashboards, as the Docker test server's `observe` profile loads it |

Nothing here names a vendor or a host: a backend is
`https://otlp.example.com`, and its token comes from the collector's
environment.

## How they are tested

- **By `go test`** (`internal/telemetry/harness_test.go`): every alert
  rule and every Prometheus query of the dashboards names only series, labels and
  label values that ariesd's instruments have, so renaming an
  instrument fails the build; the rules cover what the design says an
  operator is told of, each signal ariesd can drop among them; the
  scrape job of both configurations is the one the rules select by,
  at the address the server guide gives; and the images the Docker
  test server runs them in are pinned by digest.
- **By the Docker smoke test**, when asked
  (`ARIES_SMOKE_TELEMETRY=1 deploy/docker/smoke.sh`,
  [docs/deploy/docker.md](../../docs/deploy/docker.md#with-the-telemetry-harness)):
  a real Collector receives ariesd's metrics, traces and log records
  and a real Prometheus loads every rule, evaluates
  each without error and runs every query of the dashboard. CI does not
  run it.

Not tested automatically: the dashboards in Grafana itself, and the trace and log queries of the third, which were run by hand in the Docker test server's `observe` profile (16 Oct 2026: every series they read arrived under its name once its counter had counted, and each trace and log panel showed the server's own); a hosted backend; the
thresholds against a real server's traffic; and the two parts of
`collector.yaml` that are comments, the backend's exporter and the
filter for the audit stream, which the Collector's own `validate`
accepted once, by hand.

## Logs

ariesd's log goes to the journal, whole. When it is asked to
(`OTEL_LOGS_EXPORTER=otlp`), it also sends each line to the collector
as a record with the line's ids, codes and numbers and none of its
text ([Logs over OTLP](../../docs/server.md#logs-over-otlp)), and
`collector.yaml` carries those in a `logs` pipeline. The audit stream
of access changes is among them, and the file shows how to send it
alone.

`alerts.yaml` has a rule for log records ariesd could not export. It
has none that matches a line's `event`: its rules are over metrics, and
nothing here turns a record into one. Match `event` where the logs are
read.
