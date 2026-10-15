# Telemetry

starfixd exports nothing until asked (docs/server.md#observability). The
guide's order, and yours: a collector on the machine first; a hosted
backend straight from starfixd only where the operator does not want a
collector.

## Look (read-only)

- What is on: `journalctl -u starfixd -g telemetry -n 5 --no-pager`
  (the `telemetry on` line lists push, pull, traces and logs), and
  `sudo -n grep -vi dsn /etc/starfix/starfixd.yaml` for a `telemetry:`
  block.
- The unit's drop-ins, by name only:
  `ls /etc/systemd/system/starfixd.service.d/`. Do not print a drop-in
  or `systemctl show -p Environment` until you know it holds no token
  (`sudo -n grep -c OTEL_EXPORTER_OTLP_HEADERS FILE` prints 0).
- A collector already there: `systemctl list-units --type=service
  --no-pager | grep -i -e otel -e collector -e agent`; ask the operator
  which is theirs.

## A collector on the machine

docs/server.md#send-metrics-to-a-collector-on-the-machine, and
docs/server.md#a-collector-alert-rules-and-a-dashboard for the tested
examples in `deploy/observability` (collector.yaml, alerts.yaml,
prometheus.yaml, dashboard.json). Each step is a change, shown first:

1. **The collector**, installed as its own documentation says, with
   `deploy/observability/collector.yaml` adapted to the operator's
   backend. Its exporter's credential is the operator's to put in place;
   you never write it.
2. **starfixd pushes to it**: a drop-in for the unit with
   `Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`.
3. **Stand-ins for people in traces**: the directory
   `/var/lib/starfix/telemetry` (0700, `starfix`) and its drop-in with
   `ReadWritePaths=/var/lib/starfix/telemetry`. Or, if the operator
   wants people left out, `telemetry: principals: omit`.
4. **The listener**, for the collector to scrape, `telemetry: listen:
   127.0.0.1:9464` (docs/server.md#the-listener-metrics-to-scrape-and-health).
   That is a line under the config file's `telemetry:` block, which
   holds the database password: the operator adds it
   (`sudoedit /etc/starfix/starfixd.yaml`); a key given twice stops
   starfixd. Then check the file's mode is still `600 starfix:starfix`.
5. **Apply**: `sudo -n systemctl daemon-reload`, then `sudo -n systemctl
   restart starfixd` (drops connections for a moment: say so).
6. **Verify**: the `telemetry on` line;
   `curl -s http://127.0.0.1:9464/readyz`; the stand-ins' key made
   (`sudo -n stat -c '%a %U' /var/lib/starfix/telemetry/principals.key`,
   never its content); and that the collector received data.

With the AppArmor profiles, a collector on the machine needs nothing
more; a hosted backend needs the three lines of
docs/server.md#confine-starfixd-with-apparmor.

## A hosted backend, straight from starfixd

docs/server.md#send-metrics-straight-to-a-hosted-backend. The backend's
token is a secret: it lives only in `/etc/starfix/telemetry.env`, mode
0600, owned by root, read by systemd.

1. With a yes, make the empty file: `sudo -n install -m 0600 -o root -g
   root /dev/null /etc/starfix/telemetry.env`, and the drop-in naming it
   (`EnvironmentFile=/etc/starfix/telemetry.env`).
2. **The operator writes the two lines themselves**, in a terminal of
   their own (`sudoedit /etc/starfix/telemetry.env`): the endpoint and
   the header with the token. You give them the variable names; never
   the token, and never ask for it.
3. Check without reading: `sudo -n stat -c '%a %U' /etc/starfix/telemetry.env`
   (`600 root`) and `sudo -n grep -c '^OTEL_EXPORTER_OTLP_' /etc/starfix/telemetry.env`.
4. Reload, restart and verify as above. starfixd logs the number of
   headers, never their values.

## Logs over OTLP, and alerts

Log records over OTLP are off unless `OTEL_LOGS_EXPORTER=otlp` is set
(docs/server.md#logs-over-otlp). The log's events and fields are a
contract to alert on (docs/server.md#logs); match `event`, not `msg`.
The alert rules in `deploy/observability/alerts.yaml` are tested
against starfixd's metrics; start from them.
