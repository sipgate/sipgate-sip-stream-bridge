# Caller-ID Policy

Operator runbook for caller-ID resolution on outbound `<Dial>` calls.
Part of the v3.0 hardening + release-gate documentation set. See
[RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for the pre-merge checklist
that ties this runbook to the CI gate.

The bridge resolves the outbound caller-ID for every `<Dial>` via a
fall-back chain — TwiML `callerId` attribute beats per-tenant config
beats inbound-ANI preservation beats deployment default. If the chain
exhausts without a usable value the bridge rejects the dial with Twilio
error code `13214`.

## Symptom

- Customer reports `<Dial>` call rings at the destination but the
  caller-ID shown is unexpected (wrong number, "anonymous", or our
  default fallback rather than the customer's own DID).
- Outbound dial fails with Twilio error `13214` (caller-ID rejected).
- `sipgate_bridge_forward_failed_total{reason="caller_id_rejected"}`
  increments unexpectedly in Grafana.

## Why caller-ID matters

sipgate's SIP trunk policy validates the From-header / P-Asserted-Identity
on outbound INVITEs. A caller-ID outside the customer's verified DID set
is rejected at the trunk (407 / 403 / 603 depending on policy). Caller-ID
also affects regulatory compliance (CLIP/CLIR rules in EU; STIR/SHAKEN
attestation in NANP).

## The four-tier fallback chain

The bridge resolves the outbound caller-ID in this order. The first tier
that produces a non-empty, policy-acceptable value wins.

| Tier | Source                             | Wins when                                                            |
| ---- | ---------------------------------- | -------------------------------------------------------------------- |
| 1    | TwiML `<Dial callerId="...">`      | The customer set the attribute on the verb explicitly                |
| 2    | Inbound-ANI preservation           | TwiML attribute absent; the inbound `From` header is a usable E.164  |
| 3    | `DIAL_DEFAULT_CALLER_ID` env       | Tiers 1+2 produced no usable value                                   |
| 4    | Reject with `13214`                | All three tiers failed — fail-closed posture                         |

### Tier 1 — TwiML `callerId`

```xml
<Response>
  <Dial callerId="+4912345678">+491701234567</Dial>
</Response>
```

The bridge accepts the value verbatim if it parses as E.164. Per
sipgate trunk policy this MUST be a verified DID owned by the
`account_sid`; a non-owned number trips the trunk's CLIP filter and
returns 403/603.

### Tier 2 — inbound-ANI preservation

When `<Dial callerId>` is unset, the bridge preserves the inbound
caller's ANI by copying the inbound `From` URI's user-part into the
outbound `From`. This matches Twilio's behaviour for "answer-and-bridge"
flows where the bot answers, then forwards via `<Dial>` without
altering caller-ID semantics.

### Tier 3 — `DIAL_DEFAULT_CALLER_ID`

```env
DIAL_DEFAULT_CALLER_ID=+4912345
```

Set this for deployments where Tier 2 cannot be relied on (e.g.
machine-originated dials with no inbound leg). Must be a sipgate-verified
DID. Empty by default.

If `DIAL_CALLER_ID_COUNTRY_CODE` is also set, the bridge applies trunk
display normalisation:

```env
DIAL_CALLER_ID_COUNTRY_CODE=49
```

This rewrites a leading `+49` into the trunk-local format sipgate
expects (legacy NPN trunks may require `0049` or `49` without `+`;
verify against the specific trunk profile before setting).

### Tier 4 — reject with `13214`

If all three preceding tiers fail to produce a value, the bridge does
NOT send the INVITE. It logs `caller_id_rejected`, increments
`sipgate_bridge_forward_failed_total{reason="caller_id_rejected"}`, and
returns `13214` to the customer via the status callback.

## Per-tenant configuration

The bridge currently runs single-tenant per process — multi-tenant
caller-ID isolation is **deferred to v3.1+** (see CONTEXT
`<deferred>`). For multi-tenant deployments, run one bridge instance
per `account_sid` with that tenant's `DIAL_DEFAULT_CALLER_ID` baked
into its env.

## What triggers Twilio code 13214

The bridge emits `13214` when:

1. Tier 1 set a non-E.164 value (parse failure).
2. Tier 1 set a value the sipgate trunk rejected (403/603 received on
   the outbound INVITE).
3. Tier 2 + Tier 3 are both empty AND Tier 1 is unset.

For (2), the bridge surfaces the trunk's response code in the
`forward_failed_total{reason="caller_id_rejected"}` increment — operators
must inspect logs for the underlying `trunk_response` field to
distinguish "wrong DID" from "trunk policy issue".

## sipgate trunk caller-ID policy

Open question: **does the sipgate trunk enforce P-Asserted-Identity
matching against the registered DID set, and at what error code?**
v3.0 ships assuming `403` / `603` for CLIP rejection; this runbook
will be updated when live-test confirms.

If the operator observes a sipgate trunk rejection code NOT in
{`403`, `407`, `603`} for a caller-ID issue, escalate to trunk
operations and amend this runbook with the observation.

## Related runbooks

- [TOLL_FRAUD.md](./TOLL_FRAUD.md) — `<Dial>` allow-list (caller-ID is
  validated AFTER the toll-fraud allow-list passes).
- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation
  when a caller-ID compromise is suspected.
- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana panel for
  `forward_failed_total{reason="caller_id_rejected"}`.

## Reference

- `go/internal/config/config.go` — `DialDefaultCallerID`,
  `DialCallerIDCountryCode` fields
- Twilio error reference: `13214` (caller-ID rejected)
- RFC 8224 (STIR/SHAKEN — NANP attestation; not enforced in v3.0 but
  in scope for v3.1+)
- Open: sipgate trunk caller-ID policy — pending live test.
