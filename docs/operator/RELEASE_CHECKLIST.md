# Operator Release Checklist (v3.0+)

Operator-facing pre-merge checklist for any v3.0+ release. Tick each
item; **the PR cannot merge to main until all items below are green.**

## How to use this checklist

1. Copy the checklist below into the release PR description.
2. Each item is signed off by an operator (initials + date).
3. The CI gates (test, lint, lint-metrics, e2e, image-size-check)
   are auto-checked by `.github/workflows/docker-go.yml`; the
   operator confirms the run is green.
4. The smoke-verification items require live calls against the
   target environment (staging or canary preferred).

## Checklist

### Pre-deployment

- [ ] CI green on the merge commit
  - [ ] `make test` (`go test -race -count=3 ./...`)
  - [ ] `make lint` (golangci-lint)
  - [ ] `make lint-metrics` (cardinality + secret-mask discipline)
  - [ ] `make e2e` (8 sipp scenarios + Go integration tests)
  - [ ] `make image-size-check` (6.0 MB budget; see
        [IMAGE_SIZE.md](./IMAGE_SIZE.md))
- [ ] CHANGELOG.md updated for this release (`## [X.Y.Z] - YYYY-MM-DD`
      entry with Added / Changed / Removed / Fixed / Security
      sections per Keep a Changelog v1.1.0)
- [ ] `docs/release-notes/vX.Y.Z.md` authored — operator-facing pitch
      with new env vars, metric labels, dependencies, toolchain bumps
- [ ] Dashboard provisioned in target environment per
      [DASHBOARDS.md](./DASHBOARDS.md)
- [ ] Alert rules applied (`deploy/observability/prometheus-rules.yaml`)
- [ ] Toll-fraud guardrail config validated per
      [TOLL_FRAUD.md](./TOLL_FRAUD.md) (per-market `DIAL_ALLOWED_PREFIXES`
      recipe applied; deny-all default if `<Dial>` not in use)
- [ ] `AUTH_TOKEN` rotated since the last release per
      [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) (or scheduled-
      rotation cadence verified)

### Smoke verification (against the deployed instance)

- [ ] `/health` returns the locked 4-field JSON shape:
      `{"registered": ..., "account_sid": ..., "active_calls": ...,
      "active_forwards": ...}` (snake_case; no extra fields)
- [ ] One synthetic inbound call bridges to `WS_TARGET_URL` with the
      expected `start` / `media` / `mark` / `clear` / `stop` events
      (default-streaming path)
- [ ] One synthetic `<Dial>` call to a known-allowed prefix completes
      with `DialCallStatus=completed` (Twilio-compat status)
- [ ] One synthetic `<Dial>` call to a deny-listed prefix returns
      `21215` (toll-fraud rejection — see
      [TOLL_FRAUD.md](./TOLL_FRAUD.md))
- [ ] One synthetic REST `POST /Calls/{CallSid}.json` with
      `Status=completed` ends the call and emits the final
      `completed` status callback (see
      [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) for the
      X-Twilio-Signature verification expectation)
- [ ] Grafana dashboard shows non-zero traffic on the synthetic-call
      panels within 5 minutes of completing the smoke calls

### Graceful shutdown verification (optional but recommended)

- [ ] Trigger a rolling restart of the bridge while a synthetic call
      is in progress; verify:
  - [ ] The call drains via `DrainAll` within the 15s budget
  - [ ] A final `completed` status callback fires for the drained
        call BEFORE the SIP BYE
  - [ ] No goroutine leak warnings in the new pod within 5 minutes of
        startup

### Image size

- [ ] `make image-size-check` reports
      `image size: <NNN> bytes (limit: 6000000 bytes / ~5.7 MB)` with
      exit 0. Record the actual size in the PR description for
      release-history tracking.

### Sign-off

- [ ] Operator name + date
- [ ] (Optional) Customer-comms drafted for the release notes
- [ ] (Optional) Compliance review for any new security-affecting env
      vars (`AUTH_TOKEN`, `PUBLIC_BASE_URL`, `DIAL_ALLOWED_PREFIXES`)

---

## Scope

This checklist covers the *operational* side of a release:

- Deploy artefacts are provisioned in the target environment.
- Smoke calls succeed against the deployed instance.
- Customer-comms / compliance review are signed off.
- Configuration recipes from the runbooks are applied.

The *engineering* side is mechanically enforced by CI: a release tag
is only cut once all CI gates (test, lint, lint-metrics, e2e,
image-size-check) are green on the merge commit.

## Related runbooks

- [TOLL_FRAUD.md](./TOLL_FRAUD.md) — `<Dial>` allow-list recipes.
- [CALLER_ID.md](./CALLER_ID.md) — caller-ID fallback chain.
- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation.
- [SESSION_TIMERS.md](./SESSION_TIMERS.md) — RFC 4028 v3.0 stance.
- [IMAGE_SIZE.md](./IMAGE_SIZE.md) — image budget verification.
- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana / Prometheus
  provisioning.

## Reference

- Keep a Changelog v1.1.0 — https://keepachangelog.com/en/1.1.0/
- Twilio status-callback event vocabulary:
  https://www.twilio.com/docs/voice/api/call-resource#statuscallbackevent
