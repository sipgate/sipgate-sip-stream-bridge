# Dashboards & Alerting

Operator runbook for provisioning the v3.0 Grafana dashboard and
Prometheus alert rules. Part of the v3.0 hardening + release-gate
documentation set. See [RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md)
for the pre-merge checklist.

The bridge ships three observability artefacts under
`deploy/observability/`:

- `grafana-dashboard.json` — Grafana provisioning-format dashboard.
  Import via the Grafana UI (Dashboards → Import → upload JSON) or
  auto-load via Grafana provisioning.
- `prometheus-rules.yaml` — kube-prometheus-stack `PrometheusRule`
  CRD. Vanilla-Prometheus compatible — extract `spec.groups`.
- `README.md` — full PromQL reference + alert runbooks for operators
  on neither stack. **This file is the canonical operator runbook for
  per-alert details; the present file is the deploy/provisioning
  recipe.**

## Applying the artefacts

### kube-prometheus-stack

```bash
kubectl apply -f deploy/observability/prometheus-rules.yaml -n monitoring
```

The Prometheus operator picks up the `PrometheusRule` CRD when it
matches the running Prometheus's `ruleSelector` label selector. The
default `prometheus: kube-prometheus` label is broadly compatible;
adjust if your operator uses a different selector.

### Vanilla Prometheus

Extract `spec.groups` from `prometheus-rules.yaml` into a standalone
file (e.g. `/etc/prometheus/rules/sipgate-sip-stream-bridge.yml`) and
reference it from `prometheus.yml`:

```yaml
rule_files:
  - /etc/prometheus/rules/sipgate-sip-stream-bridge.yml
```

The rule shape (`groups[].rules[]`) is identical between the CRD and
vanilla formats; only the wrapper differs.

### Grafana — UI import

UI path: Dashboards → Import → Upload JSON →
`deploy/observability/grafana-dashboard.json`. Pick your Prometheus
datasource at the import prompt — the dashboard exposes a
`${DS_PROMETHEUS}` template variable for that purpose.

### Grafana — provisioning

Place `grafana-dashboard.json` under your Grafana provisioning
`dashboards/` directory; reference it from a provisioning YAML:

```yaml
# /etc/grafana/provisioning/dashboards/sipgate-bridge.yaml
apiVersion: 1
providers:
  - name: sipgate-bridge
    orgId: 1
    folder: SIP
    type: file
    disableDeletion: false
    updateIntervalSeconds: 60
    options:
      path: /var/lib/grafana/dashboards/sipgate-bridge
```

Then mount the directory into the Grafana pod and place
`grafana-dashboard.json` inside it.

### Helm / Argo / kustomize

The artefacts are plain YAML / JSON — wrap them per your stack:

- **Helm**: copy into `templates/` and add a `--set` toggle for the
  dashboard.
- **kustomize**: reference via `resources:` and patch label selectors
  per environment.
- **Argo CD**: include the directory as a sync source under your
  `Application` manifest.

## v3.0 dashboard import URL

The dashboard JSON is committed at
`deploy/observability/grafana-dashboard.json`. It exposes
panels for:

- Active calls (`active_calls_total` gauge)
- Forward volume + failure breakdown (LOCKED `reason` enum)
- Status-callback event rate (LOCKED `event` enum, Twilio vocabulary)
- Status-callback failure breakdown (LOCKED `reason` enum)
- RTP port pool utilisation
- REST API request rate by route + bucketed status
- TwiML modify-call outcomes
- SIP registration state
- WebSocket reconnects
- Authentication challenge type (401/407)

Refer to `deploy/observability/README.md` for the full PromQL inventory
and panel-by-panel description.

## Locked alert thresholds

| Alert                     | Threshold                                                | Sustained | Severity |
| ------------------------- | -------------------------------------------------------- | --------- | -------- |
| TollFraudActivity         | `increase(...{reason="toll_fraud"}[1h]) > 5`             | 5m        | warning  |
| StatusCallbackFailureRate | `failures / attempts > 0.10`                             | 5m        | warning  |
| RTPPortPoolPressure       | `in_use / size > 0.8`                                    | 30s       | warning  |
| GoroutineLeakSuspect      | `rate(go_goroutines[5m]) > 0` AND `go_goroutines > 1000` | 10m       | warning  |

These thresholds are LOCKED — modifying them requires an
operator-visible CHANGELOG entry. Severity is `warning` for all four;
operators may promote to `critical` per their internal SLO discipline
by patching the rule in their own overlay. Avoid editing
`prometheus-rules.yaml` directly so the provisioning artefact stays
canonical.

## Alarm threshold tuning

If false-positive rates are unacceptable in a given environment:

| Issue                                                    | Tuning lever                                                                                                                        |
| -------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `TollFraudActivity` fires under load tests               | Increase `[1h]` window OR raise the `> 5` threshold (rule patch only — DO NOT loosen the underlying allow-list per [TOLL_FRAUD.md](./TOLL_FRAUD.md)) |
| `StatusCallbackFailureRate` flaps on customer downtime   | Add `for: 15m` (extend sustain window) — but investigate the customer outage first                                                  |
| `RTPPortPoolPressure` fires on legitimate peak hours     | Increase `RTP_PORT_MAX` (extends pool); restart the bridge — see [TOLL_FRAUD.md](./TOLL_FRAUD.md) and `deploy/observability/README.md` `RTPPortPoolPressure` runbook |
| `GoroutineLeakSuspect` cannot fire (no goroutine metric) | Add a Go-runtime exporter sidecar OR substitute `process_resident_memory_bytes` per `deploy/observability/README.md` |

## Per-alert runbooks

The full per-alert prose runbook is in
`deploy/observability/README.md` (lines 188-308) and predates this
operator-runbook directory. The runbook directory cross-links into it
rather than duplicating prose.

| Alert                     | Runbook                                                          |
| ------------------------- | ---------------------------------------------------------------- |
| TollFraudActivity         | `deploy/observability/README.md` + [TOLL_FRAUD.md](./TOLL_FRAUD.md) |
| StatusCallbackFailureRate | `deploy/observability/README.md` (under "Runbooks")              |
| RTPPortPoolPressure       | `deploy/observability/README.md` (under "Runbooks")              |
| GoroutineLeakSuspect      | `deploy/observability/README.md` (under "Runbooks")              |

## Goroutine metric caveat

The bridge's `/metrics` endpoint uses a custom
`prometheus.NewRegistry` — `go_goroutines` is NOT exposed by `/metrics`
on the bridge process itself (this is a v2.1 invariant; see
`go/internal/observability/metrics.go` header).

For `GoroutineLeakSuspect` to fire, operators must scrape goroutine
counts via one of:

- A separate Go-runtime exporter sidecar that imports and exposes the
  default `prometheus.DefaultRegisterer` (most common for k8s
  deployments).
- `process_exporter` / `node_exporter` with the
  `process_resident_memory_bytes` rule as a coarser substitute.

If neither is available, replace the alert with the
`ProcessMemoryGrowth` substitute from
`deploy/observability/README.md`.

## Verifying provisioning

After applying the artefacts:

1. Grafana dashboard appears in the configured folder; panels render
   with data (no "no data" placeholders) when the bridge has been
   running > 5 minutes.
2. Prometheus rules are loaded:
   ```bash
   kubectl exec -n monitoring prometheus-... -- \
     promtool query instant http://localhost:9090 'ALERTS{alertstate="firing"}'
   ```
3. A synthetic toll-fraud rejection (drive a `<Dial>` to a denied
   prefix 6 times in an hour) fires `TollFraudActivity` within 5
   minutes.

## Related runbooks

- [TOLL_FRAUD.md](./TOLL_FRAUD.md) — `TollFraudActivity` alert
  response.
- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation
  (the alert pattern often correlates with compromise).
- [IMAGE_SIZE.md](./IMAGE_SIZE.md) — Docker image budget (orthogonal
  to dashboards but part of the same release-gate set).
- [RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) — the
  "Dashboard provisioned" item in the operator pre-deploy checklist.

## Reference

- `deploy/observability/grafana-dashboard.json`
- `deploy/observability/prometheus-rules.yaml`
- `deploy/observability/README.md` (full PromQL + per-alert runbooks)
- Grafana provisioning docs:
  https://grafana.com/docs/grafana/latest/administration/provisioning/
- Prometheus rule_files spec:
  https://prometheus.io/docs/prometheus/latest/configuration/recording_rules/
- kube-prometheus-stack PrometheusRule CRD:
  https://prometheus-operator.dev/docs/operator/api/#prometheusrule
