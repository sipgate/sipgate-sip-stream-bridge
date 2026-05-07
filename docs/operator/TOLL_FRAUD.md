# Toll Fraud Guardrails

Operator runbook for the v3.0 toll-fraud guardrail subsystem. Part of the
v3.0 hardening + release-gate documentation set. See
[RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for the pre-merge checklist
that ties this runbook to the CI gate.

The bridge enforces a SIP-layer allow-list on every outbound `<Dial>`
target via the `DIAL_ALLOWED_PREFIXES` config. Default behaviour is
**deny-all** — an unset or empty `DIAL_ALLOWED_PREFIXES` rejects every
outbound dial with Twilio error code `21215` and increments
`sipgate_bridge_forward_failed_total{reason="toll_fraud"}`.

## Symptom

The Grafana `TollFraudActivity` alert fires (`increase(forward_failed_total{reason="toll_fraud"}[1h]) > 5`,
sustained 5m, severity warning) — or operators notice rejected legitimate
dials in customer reports.

## Common causes

1. **Compromised customer bot** — credentials leaked; an attacker is
   sending `<Dial>` to high-cost destinations (premium-rate numbers,
   international long-distance not on the allow-list).
2. **Misconfigured `DIAL_ALLOWED_PREFIXES`** — the legitimate destination
   prefix was not added when the customer use case launched.
3. **Empty allow-list in production** — the deploy did not set
   `DIAL_ALLOWED_PREFIXES` at all, so every `<Dial>` returns 21215.

## First investigation

1. Filter logs by `reason=toll_fraud` and the affected `account_sid` (when
   present) to identify the destination prefix(es) being rejected:
   ```bash
   kubectl logs -l app=sipgate-bridge --since=1h \
     | jq 'select(.reason=="toll_fraud") | {account_sid, to_uri, callee}'
   ```
2. Group by destination prefix to spot patterns:
   ```bash
   kubectl logs -l app=sipgate-bridge --since=1h \
     | jq -r 'select(.reason=="toll_fraud") | .callee // .to_uri' \
     | awk -F'@' '{print $1}' | sort | uniq -c | sort -rn
   ```
3. Cross-reference with `forward_attempts_total` to see whether the
   rejection rate is climbing relative to legitimate traffic.

## Mitigation / Resolution

### If the destination is legitimate (matches a known customer use case)

Add the prefix to `DIAL_ALLOWED_PREFIXES`; restart the bridge:

```bash
# Edit the deployment env (k8s example)
kubectl set env deployment/sipgate-bridge \
  DIAL_ALLOWED_PREFIXES="+49,+1,+44"

# Rolling restart
kubectl rollout restart deployment/sipgate-bridge
```

### If the destination is suspicious

Premium-rate prefix, unusual country code, or matches a known fraud
campaign — rotate the customer's `AUTH_TOKEN` immediately and follow
[INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) for the credential rotation
procedure. Investigate intrusion via REST API access logs (look for
unexpected source IPs or unusual call rates).

## Per-market allow-list recipes

Apply the recipe matching the customer's calling pattern. Prefixes are
E.164 with leading `+`; CSV separator is comma; no whitespace.

### Germany (sipgate primary market)

```env
DIAL_ALLOWED_PREFIXES=+49
```

### USA / Canada (NANP)

```env
DIAL_ALLOWED_PREFIXES=+1
```

### UK

```env
DIAL_ALLOWED_PREFIXES=+44
```

### DACH region (Germany + Austria + Switzerland)

```env
DIAL_ALLOWED_PREFIXES=+49,+43,+41
```

### Western Europe baseline

```env
DIAL_ALLOWED_PREFIXES=+49,+43,+41,+33,+31,+32,+34,+39,+44
```

### Deny-all baseline (default — `<Dial>` disabled)

```env
DIAL_ALLOWED_PREFIXES=
```

A deployment with `<Dial>` *not in use* SHOULD leave this empty so any
inadvertent TwiML response containing `<Dial>` is rejected at the SIP
layer.

## Reading `forward_failed_total{reason="toll_fraud"}`

The `forward_failed_total` counter has a 12-value bounded `reason`
enum (LOCKED — enforced by `cmd/lint-metrics`):

`busy`, `no_answer`, `rejected`, `unreachable`, `codec_mismatch`,
`toll_fraud`, `rate_limit`, `caller_id_rejected`, `auth_failed`,
`trunk_5xx`, `timeout`, `error`.

Operators reading the Grafana panel should compare the `toll_fraud`
slice against `rate_limit` and `caller_id_rejected` slices — sustained
non-zero rates across all three usually indicate a broken deployment
config rather than an active attack.

PromQL recipes:

```promql
# Toll-fraud rate per minute
rate(sipgate_bridge_forward_failed_total{reason="toll_fraud"}[1m])

# Top affected accounts in the last hour (only meaningful if account_sid
# is present in your custom relabel pipeline; the metric itself does
# NOT carry account_sid as a label per cardinality discipline)
sum(increase(sipgate_bridge_forward_failed_total{reason="toll_fraud"}[1h]))

# Rejection ratio over total dial attempts
sum(rate(sipgate_bridge_forward_failed_total{reason="toll_fraud"}[5m]))
  / sum(rate(sipgate_bridge_forward_attempts_total[5m]))
```

## Escalation

If the pattern matches a known fraud campaign or originates from
multiple `account_sid`s simultaneously, page the on-call security
engineer. Capture the affected destination prefixes, source IPs (REST
API access logs), and a 15-minute log window before paging.

Internal escalation contacts: see sipgate operator wiki (placeholder —
operator team to fill at deploy time).

## Related runbooks

- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation when
  a leak is suspected.
- [CALLER_ID.md](./CALLER_ID.md) — caller-ID policy interactions with
  `<Dial>` (some toll-fraud rejections also implicate caller-ID rules).
- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana dashboard import + alert
  threshold tuning.

## Reference

- `deploy/observability/README.md` — alert thresholds + PromQL inventory
- `go/internal/config/config.go` — `DialAllowedPrefixes` field (env
  `DIAL_ALLOWED_PREFIXES`, CSV)
- Twilio error code reference: `21215` (geo-permissions — used for
  toll-fraud rejections in v3.0)
