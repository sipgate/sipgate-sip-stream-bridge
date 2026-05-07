# sipgate-sip-stream-bridge — Observability Artifacts

Operator dashboards and alert rules for v3.0. Three artifacts in this
directory:

- `grafana-dashboard.json` — Grafana provisioning-format dashboard. Import via
  the Grafana UI (Dashboards → Import → upload JSON) or auto-load via Grafana
  provisioning.
- `prometheus-rules.yaml` — kube-prometheus-stack `PrometheusRule` CRD.
  Vanilla-Prometheus compatible — extract `spec.groups`.
- This README — copy-paste PromQL reference + alert runbooks for operators on
  neither stack.

The alert thresholds encoded here are LOCKED at the operator-runbook level.
Do not modify without an operator-visible CHANGELOG entry.

## Applying the Artifacts

### kube-prometheus-stack

```bash
kubectl apply -f deploy/observability/prometheus-rules.yaml -n monitoring
```

The Prometheus operator picks up the `PrometheusRule` CRD when it matches the
running Prometheus's `ruleSelector` label selector. The default
`prometheus: kube-prometheus` label is broadly compatible; adjust if your
operator uses a different selector.

### Vanilla Prometheus

Extract `spec.groups` from `prometheus-rules.yaml` into a standalone file (e.g.
`/etc/prometheus/rules/sipgate-sip-stream-bridge.yml`) and reference it from
`prometheus.yml`:

```yaml
rule_files:
  - /etc/prometheus/rules/sipgate-sip-stream-bridge.yml
```

The rule shape (`groups[].rules[]`) is identical between the CRD and vanilla
formats; only the wrapper differs.

### Grafana

UI path: Dashboards → Import → Upload JSON → `grafana-dashboard.json`. Pick
your Prometheus datasource at the import prompt — the dashboard exposes a
`${DS_PROMETHEUS}` template variable for that purpose.

Provisioning path: place `grafana-dashboard.json` under your Grafana
provisioning `dashboards/` directory; reference it from a provisioning YAML.

## Locked Alert Thresholds

| Alert                     | Threshold                                                   | Sustained | Severity |
| ------------------------- | ----------------------------------------------------------- | --------- | -------- |
| TollFraudActivity         | `increase(...{reason="toll_fraud"}[1h]) > 5`                | 5m        | warning  |
| StatusCallbackFailureRate | `failures / attempts > 0.10`                                | 5m        | warning  |
| RTPPortPoolPressure       | `in_use / size > 0.8`                                       | 30s       | warning  |
| GoroutineLeakSuspect      | `rate(go_goroutines[5m]) > 0` AND `go_goroutines > 1000`    | 10m       | warning  |

Severity is `warning` for all four — they are indicators of degradation, not
outages. Operators may promote to `critical` per their internal SLO discipline.

## PromQL Reference

Copy-paste expressions for ad-hoc Grafana panels or Prometheus queries.

### Active Calls

```promql
active_calls_total
```

(NB: `active_calls_total` is a gauge despite the `_total` suffix — the
suffix is preserved for compatibility with operators who already
configured this name. See metrics.go header comment.)

### Forward Volume

```promql
# Cumulative <Dial> attempts that passed guardrails
sipgate_bridge_forward_attempts_total

# Cumulative answered (200 OK + ACK + first frame relayed)
sipgate_bridge_forward_success_total

# Cumulative failures, by reason bucket
sum(rate(sipgate_bridge_forward_failed_total[5m])) by (reason)
```

### Forward Failure Reason Breakdown

```promql
sum(rate(sipgate_bridge_forward_failed_total[5m])) by (reason)
```

Reason enum (LOCKED):
`busy | no_answer | rejected | unreachable | codec_mismatch | toll_fraud |`
`rate_limit | caller_id_rejected | auth_failed | trunk_5xx | timeout | error`

### Status Callback Event Rate

```promql
sum(rate(sipgate_bridge_status_callback_attempts_total[5m])) by (event)
```

Event enum (LOCKED, Twilio vocabulary):
`initiated | ringing | answered | in-progress | completed | busy | failed |`
`no-answer | canceled`

### Status Callback Failure Reason Breakdown

```promql
sum(rate(sipgate_bridge_status_callback_failures_total[5m])) by (reason)
```

Reason enum (LOCKED):
`timeout | 4xx | 5xx | connect_error | exhausted_retries | ssrf_rejected |`
`queue_full`

### RTP Port Pool Utilization

```promql
sipgate_bridge_rtp_port_pool_in_use / sipgate_bridge_rtp_port_pool_size
```

### RTP Port Pool Acquire Failures

```promql
rate(sipgate_bridge_rtp_port_acquire_failures_total[5m])
```

### Goroutine Count (Leak Indicator)

```promql
go_goroutines
```

### REST API Request Rate (by Bucketed Status)

```promql
sum(rate(api_requests_total[5m])) by (route, status)
```

Route enum (LOCKED): `list_calls | get_call | modify_call | health | metrics | unknown`
Method enum: `GET | POST | PUT | DELETE`
Status enum: `2xx | 4xx | 5xx`

### TwiML Modify Outcomes

```promql
sum(rate(twiml_modify_total[5m])) by (kind, outcome)
```

Kind enum: `twiml | url | status_completed`
Outcome enum: `ok | parse_error | fetch_error | invalid_params | terminated | hangup`

### SIP Registration State

```promql
sip_registration_status
```

(0 = unregistered/failed, 1 = registered)

### WebSocket Reconnects

```promql
rate(ws_reconnect_attempts_total[5m])
```

### SIP OPTIONS Keepalive Failures

```promql
rate(sip_options_failures_total[5m])
```

### Authentication Challenge Type (Outbound INVITE)

```promql
sum(rate(sipgate_bridge_auth_challenge_kind_total[5m])) by (kind)
```

Kind enum: `401` (UAS auth) | `407` (proxy auth)

## Runbooks

### TollFraudActivity

**Symptom:** alert fires when toll-fraud guardrails reject more than 5 dial
attempts in the past hour.

**Common causes:**

1. **Compromised customer bot** — credentials leaked; attacker is sending
   `<Dial>` to high-cost destinations (premium-rate numbers, international
   long-distance).
2. **Misconfigured `DIAL_ALLOWED_PREFIXES`** — legitimate destinations are
   not in the allow-list.

**First investigation:**

1. Filter logs by `reason=toll_fraud` and the affected `account_sid` (when
   present). Identify the destination prefix(es) being rejected.
2. If destination is legitimate (matches a known customer use case): add the
   prefix to `DIAL_ALLOWED_PREFIXES`; restart the bridge.
3. If destination is suspicious (premium-rate / unusual country code / pattern
   matches known fraud campaign): rotate the customer's `AUTH_TOKEN`
   immediately; investigate intrusion via REST API access logs (look for
   unexpected source IPs or unusual call rates).

**Escalation:** if the pattern matches a known fraud campaign or originates
from multiple `account_sid`s simultaneously, page the on-call security
engineer.

### StatusCallbackFailureRate

**Symptom:** ≥ 10% of status callbacks fail over a 5-minute window.

**Common causes:** customer host down; SSRF guard rejecting URLs (RFC1918 /
link-local / localhost — usually customer misconfig); signature verification
failures (customer rotated their `AUTH_TOKEN` without telling us — but note
the bridge signs OUTBOUND callbacks; this is more usually a customer's
verification middleware mismatch); network egress issues from the bridge's
deployment environment.

**First investigation:** look at
`sipgate_bridge_status_callback_failures_total{reason}` to see which failure
mode dominates.

| reason              | Likely cause                                                |
| ------------------- | ----------------------------------------------------------- |
| `ssrf_rejected`     | customer URL config issue (RFC1918 / localhost / 169.254/16)|
| `timeout`           | customer host slow or unresponsive                          |
| `connect_error`     | DNS / TCP / TLS — customer host or network broken           |
| `4xx`               | customer-side application error (auth / route / body)       |
| `5xx`               | customer-side server error                                  |
| `exhausted_retries` | retried 3× with exp-backoff and still failed                |
| `queue_full`        | per-call queue depth (64) exceeded — backpressure indicator |

**Escalation:** persistent > 10% across multiple customers indicates a
bridge-side issue (egress firewall, DNS, time skew affecting signature
windows). Otherwise it is a per-customer issue; contact the affected customer.

### RTPPortPoolPressure

**Symptom:** > 80% RTP port pool utilization for ≥ 30 seconds.

**Common causes:**

1. **Traffic spike** (legitimate peak hour or DoS).
2. **`<Dial>` traffic doubled** — each dial consumes two ports (caller leg +
   callee leg).
3. **`RTP_PORT_MAX` set too low** for current concurrency.

**First investigation:** check current concurrent calls
(`active_calls_total`) and dial rate
(`rate(sipgate_bridge_forward_attempts_total[1m])`). Compute peak port
demand: `2 * peak_concurrent_calls`. Compare with configured pool size
(`sipgate_bridge_rtp_port_pool_size`).

**Mitigation:**

- Increase `RTP_PORT_MAX` (extends pool); restart the bridge for the new
  range to take effect.
- Scale horizontally (more bridge instances behind the SIP-trunk DNS).
- Rate-limit at the SIP layer via `DIAL_MAX_PER_MINUTE`.

### GoroutineLeakSuspect

**Symptom:** goroutine count > 1000 AND growing for ≥ 10 minutes.

**Important caveat:** the bridge's metrics endpoint uses a custom
`prometheus.NewRegistry` — `go_goroutines` is NOT exposed by `/metrics` on
the bridge process itself (this is the v2.1 invariant; see
`go/internal/observability/metrics.go` header). For this rule to fire,
operators must scrape goroutine counts via one of:

- A separate Go-runtime exporter sidecar that imports and exposes the
  default `prometheus.DefaultRegisterer` (most common for k8s deployments).
- `process_exporter` or `node_exporter` with the
  `process_resident_memory_bytes` rule as a coarser substitute.

If neither is available, replace the alert with:

```yaml
- alert: ProcessMemoryGrowth
  expr: rate(process_resident_memory_bytes{job="sipgate-sip-stream-bridge"}[5m]) > 0
        and process_resident_memory_bytes{job="sipgate-sip-stream-bridge"} > 1e9
  for: 10m
```

**Common causes** (whichever signal you have):

- Stuck `StatusClient` retry loops (customer host slow + retry queue depth 64).
- Unclosed dial-leg `perCall` entries.
- Abandoned WS reconnect goroutines.

**First investigation:** capture goroutine stacks via
`/debug/pprof/goroutine?debug=2` (NOTE: pprof endpoint is not exposed in v3.0
unless the bridge was started with the relevant flag — check). If pprof is
unavailable, inspect logs for repeating "DrainAndClose timeout" or
"retry exhausted" patterns; capture stacks via `SIGQUIT` in a non-production
replica.

**Mitigation:** trigger a graceful restart (`SIGTERM`) — the drain budget
releases stuck goroutines; the new process starts cleanly.

## Metric Inventory

Generated from `go/internal/observability/metrics.go` (cross-checked
2026-05-04). Update this table when new collectors are added.

### Sub-`sipgate_bridge_` namespace

| Metric                                                | Type      | Labels   | Description                                            |
| ----------------------------------------------------- | --------- | -------- | ------------------------------------------------------ |
| `sipgate_bridge_forward_attempts_total`               | counter   | —        | Outbound `<Dial>` attempts that passed guardrails     |
| `sipgate_bridge_forward_success_total`                | counter   | —        | Outbound dials that reached answered state            |
| `sipgate_bridge_forward_failed_total`                 | counter   | reason   | Outbound dial failures bucketed by reason              |
| `sipgate_bridge_forward_duration_seconds`             | histogram | outcome  | End-to-end `<Dial>` duration                          |
| `sipgate_bridge_auth_challenge_kind_total`            | counter   | kind     | Outbound INVITE digest challenge type (401/407)       |
| `sipgate_bridge_rtp_port_pool_in_use`                 | gauge     | —        | Current count of allocated RTP ports                  |
| `sipgate_bridge_rtp_port_pool_size`                   | gauge     | —        | Configured pool size (RTP_PORT_MAX − RTP_PORT_MIN + 1)|
| `sipgate_bridge_rtp_port_acquire_failures_total`      | counter   | —        | Pool-exhausted AcquirePort calls                      |
| `sipgate_bridge_status_callback_attempts_total`       | counter   | event    | Status-callback POST attempts                         |
| `sipgate_bridge_status_callback_failures_total`       | counter   | reason   | Status-callback failures bucketed by reason            |

### Un-namespaced (legacy v2.x collectors)

| Metric                                | Type      | Labels                  | Description                                            |
| ------------------------------------- | --------- | ----------------------- | ------------------------------------------------------ |
| `active_calls_total`                  | gauge     | —                       | Currently active CallSessions (gauge despite suffix)   |
| `sip_registration_status`             | gauge     | —                       | 1 = registered, 0 = unregistered/failed               |
| `rtp_packets_received_total`          | counter   | —                       | Total RTP packets from the SIP caller                 |
| `rtp_packets_sent_total`              | counter   | —                       | Total RTP packets to the SIP caller                   |
| `ws_reconnect_attempts_total`         | counter   | —                       | Total WebSocket reconnect attempts                     |
| `mark_echoed_total`                   | counter   | —                       | Mark echo events sent to the WS server                |
| `clear_received_total`                | counter   | —                       | Clear events received from the WS server              |
| `sip_options_failures_total`          | counter   | —                       | OPTIONS keepalive failures                            |
| `api_requests_total`                  | counter   | route, method, status   | REST API requests, bucketed                            |
| `api_request_duration_seconds`        | histogram | route                   | REST API handler latency                              |
| `twiml_modify_total`                  | counter   | kind, outcome           | TwiML modify-call invocations                          |

### Bounded label enums

All `*Vec` collectors enforce bounded cardinality at the call site.
The `cmd/lint-metrics` CI binary lints these enums against allow-list
comments adjacent to each declaration in `metrics.go`.

| Metric label                                       | Allow-listed values                                                                                                  |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `forward_failed_total{reason}`                     | `busy`, `no_answer`, `rejected`, `unreachable`, `codec_mismatch`, `toll_fraud`, `rate_limit`, `caller_id_rejected`, `auth_failed`, `trunk_5xx`, `timeout`, `error` |
| `forward_duration_seconds{outcome}`                | `answered`, `no_answer`, `busy`, `error`                                                                            |
| `auth_challenge_kind_total{kind}`                  | `401`, `407`                                                                                                         |
| `status_callback_attempts_total{event}`            | `initiated`, `ringing`, `answered`, `in-progress`, `completed`, `busy`, `failed`, `no-answer`, `canceled`           |
| `status_callback_failures_total{reason}`           | `timeout`, `4xx`, `5xx`, `connect_error`, `exhausted_retries`, `ssrf_rejected`, `queue_full`                        |
| `api_requests_total{route, method, status}`        | route ∈ {`list_calls`, `get_call`, `modify_call`, `health`, `metrics`, `unknown`}; method ∈ {`GET`, `POST`, `PUT`, `DELETE`}; status ∈ {`2xx`, `4xx`, `5xx`} |
| `twiml_modify_total{kind, outcome}`                | kind ∈ {`twiml`, `url`, `status_completed`}; outcome ∈ {`ok`, `parse_error`, `fetch_error`, `invalid_params`, `terminated`, `hangup`} |

## Cross-Reference

- Production metric source-of-truth: `go/internal/observability/metrics.go`
- Cardinality + secret-mask lint: `go/cmd/lint-metrics/`
