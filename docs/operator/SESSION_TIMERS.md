# Session Timers (RFC 4028)

Operator runbook for the v3.0 stance on SIP session timers (RFC 4028).
Part of the v3.0 hardening + release-gate documentation set. See
[RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for the pre-merge
checklist.

## v3.0 stance: reject `Require: timer` with BYE

The bridge does **not** implement RFC 4028 session refreshes in v3.0.
When an inbound INVITE (or re-INVITE) carries `Require: timer` AND the
sipgate trunk does not strip it before delivery, the bridge:

1. Logs the rejection at warn level with `reason=session_timer_required`.
2. Sends BYE to terminate the call cleanly (rather than `420 Bad
   Extension`, which leaves the dialog partially established).
3. Increments `sipgate_bridge_forward_failed_total{reason="error"}` (no
   dedicated label per cardinality discipline; the rejection appears
   in logs with the `session_timer_required` reason field).

## Why v3.0 ships without refresh

RFC 4028 mandates a refresh goroutine per dialog when the peer requires
session timers. Implementing this correctly requires:

- Per-leg refresh timer with a CSPRNG'd jitter (avoid synchronised
  refresh storms).
- Re-INVITE generation with `Session-Expires` and `Min-SE` semantics.
- Coordination with the existing `terminate(reason)` single entry-point
  on `bridge.CallSession` so a refresh failure terminates the dialog
  cleanly.
- Test coverage including `Min-SE` boundary cases and refresher
  selection (UAC vs UAS).

This is non-trivial work that was deliberately deferred for v3.0.
v3.0 ships with the conservative "reject and BYE" stance; v3.1 will
add the refresh goroutine if/when the sipgate trunk or downstream
peers demonstrate enforcement of session timers.

## Open question — does sipgate enforce session timers?

**Does the sipgate trunk strip `Require: timer` before delivery to the
bridge, or pass it through?** This is unresolved at v3.0 ship time and
needs a live test against production traffic.

If the trunk strips it: the bridge never sees `Require: timer` and the
v3.0 reject-with-BYE path is never exercised in production traffic. The
runbook entry exists as a defensive measure for when downstream peers
(end-customer SIP UAs reachable via `<Dial>`) require timers.

If the trunk passes it through: every inbound call from a peer that
requires session timers will be BYE'd by v3.0. The operator MUST
escalate to product / sipgate-trunk operations to either:

- Configure the trunk to strip `Require: timer` (preferred — turns the
  v3.0 bridge into a session-timer-transparent intermediary).
- Accept that those peers cannot bridge via v3.0 and track session-timer
  enforcement as a v3.1 blocking requirement.

## Symptom

The bridge logs `session_timer_required` rejections and BYEs the call
unexpectedly. Operator sees the customer's call drop without an obvious
cause; the WS log shows `stop` events without preceding hangup TwiML.

```bash
kubectl logs -l app=sipgate-bridge --since=1h \
  | jq 'select(.reason=="session_timer_required") | {ts: .time, callid: .call_sid, from: .from_uri, to: .to_uri}'
```

## Common causes

1. **Downstream `<Dial>` target requires session timers** — common with
   enterprise PBX endpoints; less common with consumer mobile networks.
2. **sipgate trunk passes `Require: timer` through** — would surface as
   inbound rejections; escalate to trunk operations.
3. **A test fixture is sending `Require: timer` deliberately** — sipp
   or another harness simulating an RFC-4028-strict UA.

## First investigation

1. Confirm the rejection reason:
   ```bash
   kubectl logs -l app=sipgate-bridge --since=1h \
     | jq 'select(.reason=="session_timer_required") | {direction: .direction, peer: .peer}'
   ```
   - `direction=inbound` AND `peer=<sipgate>`: the trunk is passing
     timers through — escalate to trunk operations.
   - `direction=outbound`: a `<Dial>` target requires timers — coordinate
     with the customer to whitelist a different target or wait for v3.1.

2. Capture a SIP trace at the relevant interface (sngrep / homer / pcap).
   Confirm the literal `Require: timer` header on the INVITE.

3. Cross-reference with `forward_failed_total` to estimate the blast
   radius. If session-timer-induced rejections account for >1% of
   total dial volume, escalate to product as a v3.1 priority bump.

## Mitigation

### Short-term (v3.0)

- Configure the sipgate trunk to strip `Require: timer` on inbound
  delivery (where applicable).
- For outbound `<Dial>`: coordinate with the destination operator to
  remove session-timer enforcement, OR accept that those destinations
  cannot bridge via v3.0.
- Operator-facing communication: explain v3.0 stance to affected
  customers; reference the v3.1 timeline.

### Long-term (v3.1)

- v3.1 will add a refresh goroutine. The implementation will:
  - Honour `Session-Expires` and `Min-SE` per RFC 4028.
  - Use the existing `terminate(reason)` single entry-point on
    `bridge.CallSession` for refresh-failure cleanup.
  - Ship a metric `sip_session_timer_rejected_total` (if v3.1 still
    exposes a fall-back rejection path).

## Monitoring

There is no dedicated session-timer metric in v3.0. The rejection
appears as a log emission with `reason=session_timer_required`. To
detect it as a Prometheus metric, operators may add a log-based
counter via vector / fluent-bit / loki-ruler:

```yaml
# loki-ruler example (conceptual)
- alert: SessionTimerRejection
  expr: sum(rate({app="sipgate-bridge"} |= "session_timer_required" [5m])) > 0
  for: 10m
  annotations:
    runbook: docs/operator/SESSION_TIMERS.md
```

If session-timer rejection is a sustained pattern, escalate to the
v3.1 priority queue.

## Related runbooks

- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation
  (unrelated to session timers, but the runbook pattern is the same).
- [TOLL_FRAUD.md](./TOLL_FRAUD.md) — toll-fraud guardrails (orthogonal
  to session timers).
- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana dashboard import.

## Reference

- RFC 4028 (Session Timers in SIP) — https://datatracker.ietf.org/doc/html/rfc4028
- v3.1 backlog: session-timer refresh goroutine (deferred from v3.0).
- Open: sipgate trunk session-timer behaviour — pending live test.
