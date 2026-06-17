# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog v1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file is the canonical developer audit trail. The operator-facing
release pitch lives at `docs/release-notes/v<version>.md` and is a
derived view; if the two diverge, the CHANGELOG wins.

## [Unreleased]

## [3.1.0] - 2026-06-17

### Added

- **Voice-URL webhook** (`VOICE_URL` / `VOICE_FALLBACK_URL`): Twilio-compatible
  alternative to `WS_TARGET_URL`. The bridge POSTs call metadata to the
  configured URL before answering (pre-answer), parses the TwiML
  `<Connect><Stream url="..."><Parameter .../>` response, and uses the
  per-call WS URL returned. Falls back to `VOICE_FALLBACK_URL` on HTTP 5xx
  or network error. `X-Twilio-Signature` HMAC signing on all outbound POSTs.
  `http://localhost` and `http://127.0.0.1` allowed for local dev.
  `WS_TARGET_URL` and `VOICE_URL` are mutually exclusive (XOR enforced at
  startup). Implemented in both Go and Node.js.

### Fixed

- `make image-size-check` now measures the gzip-compressed saved
  tarball (`docker save … | gzip -9 | wc -c`). The previous raw
  `wc -c` measurement reported wildly different numbers depending on
  the daemon — Docker Desktop on macOS returns layers already
  gzipped inside the tarball (~5.5 MB), while Linux Docker returns
  them uncompressed (~13.5 MB) for the same image. The new metric
  matches the registry-side compressed bytes that ghcr.io stores
  and that K8s pulls at deploy time, and produces the same number
  on macOS and Linux. The 6.0 MB budget is unchanged.

## [3.0.0] - 2026-05-XX

Mid-Call REST Control + B2BUA `<Dial>` milestone. v3.0 layers a
100%-Twilio-compatible HTTP REST API for modifying calls already in
progress on top of v2.1's SIP↔WebSocket media bridge. Default
behaviour is preserved: every inbound call auto-answers and streams
to `WS_TARGET_URL` (zero-config v2.1 parity).

### Added

- REST API at `/2010-04-01/Accounts/{AccountSid}/Calls{,/{CallSid}}.json`:
  - `GET /Calls.json` — list active + recently-completed calls.
  - `GET /Calls/{CallSid}.json` — fetch single call.
  - `POST /Calls/{CallSid}.json` — modify in-flight call via `Twiml=` /
    `Url=` / `Status=completed`.
- TwiML verbs: `<Hangup/>`, `<Dial>` + `<Number>` with full Twilio
  attribute parity.
- B2BUA call forwarding via shared `Agent.Client` for UDP source-port
  pinhole survival.
- Toll-fraud guardrails: `DIAL_ALLOWED_PREFIXES` allow-list
  (default-deny; enforced at the SIP layer).
- Caller-ID resolution chain: TwiML `callerId` → preserve inbound ANI
  → `DIAL_DEFAULT_CALLER_ID` → reject `13214`.
- Privacy gate: WS stream emits `stop reason="dial-forward"` BEFORE
  outbound INVITE on `<Dial>` (verified by the e2e suite).
- Status callbacks with monotonic `SequenceNumber` and 3× exp-backoff
  retry.
- X-Twilio-Signature HMAC-SHA1 signer; golden vectors against
  twilio-python + twilio-node.
- SSRF guard for status-callback URLs (rejects RFC1918 / link-local /
  localhost).
- Operator-trusted default StatusCallback (`STATUS_CALLBACK_DEFAULT_URL`)
  that bypasses SSRF guard for fixed operator-side endpoints.
- v3.0 env vars: `AUTH_TOKEN`, `PUBLIC_BASE_URL`,
  `DIAL_ALLOWED_PREFIXES`, `DIAL_DEFAULT_CALLER_ID`,
  `DIAL_CALLER_ID_COUNTRY_CODE`, `DIAL_RING_TIMEOUT_S`,
  `DIAL_MAX_PER_SESSION`, `DIAL_MAX_PER_MINUTE`,
  `STATUS_CALLBACK_DEFAULT_URL`, `STATUS_CALLBACK_DEFAULT_METHOD`,
  `STATUS_CALLBACK_DEFAULT_EVENTS`, `SIP_LISTEN_ADDR`,
  `SIP_OUTBOUND_TARGET_PORT`.
- Prometheus metrics (all bounded label enums):
  `api_requests_total{route, method, status}`,
  `api_request_duration_seconds{route}`,
  `twiml_modify_total{kind, outcome}`,
  `sipgate_bridge_forward_attempts_total`,
  `sipgate_bridge_forward_success_total`,
  `sipgate_bridge_forward_failed_total{reason}`,
  `sipgate_bridge_forward_duration_seconds{outcome}`,
  `sipgate_bridge_auth_challenge_kind_total{kind}`,
  `sipgate_bridge_status_callback_attempts_total{event}`,
  `sipgate_bridge_status_callback_failures_total{reason}`,
  `sipgate_bridge_rtp_port_pool_size`,
  `sipgate_bridge_rtp_port_pool_in_use`,
  `sipgate_bridge_rtp_port_acquire_failures_total`.
- CI lint binary `cmd/lint-metrics` enforcing cardinality + secret-mask
  discipline.
- Operator runbook under `docs/operator/` — 7 files:
  `TOLL_FRAUD.md`, `CALLER_ID.md`, `INCIDENT_RESPONSE.md`,
  `SESSION_TIMERS.md`, `IMAGE_SIZE.md`, `DASHBOARDS.md`,
  `RELEASE_CHECKLIST.md`.
- E2E sipp scenarios under `tests/e2e/sipp/` (8 build-blocking
  scenarios).
- E2E Go integration tests under `go/test/e2e/` covering shutdown,
  privacy gate, goroutine leak, CANCEL/BYE race.
- Grafana dashboard + Prometheus rule artefacts under
  `deploy/observability/`.
- `Makefile` targets `e2e` and `image-size-check`.
- CI gates: lint-metrics, build-blocking 8-scenario sipp e2e,
  image-size-check (6.0 MB budget — see `docs/operator/IMAGE_SIZE.md`).
- `CHANGELOG.md` (this file) at project root, Keep a Changelog v1.1.0
  format.
- `docs/release-notes/v3.0.0.md` operator-facing release summary.

### Changed

- Toolchain: Go 1.24 → 1.26.
- Dependency bump: `github.com/emiago/sipgo` v1.2.0 → v1.3.0 (auto-401/
  407 in WaitAnswer).
- Direct dependency added: `github.com/go-chi/chi/v5 v5.2.5` (REST
  router).
- Direct dependency added: `golang.org/x/tools v0.44.0` (lint-metrics
  binary uses `go/packages`).
- `/health` JSON locked to 4 fields (`registered`, `account_sid`,
  `active_calls`, `active_forwards`); snake_case.
- Drain budget extended from 10 s (v2.1) to 15 s.
- Image-size budget set to 6.0 MB after measuring 5,466,112 bytes for
  the v3.0 baseline; a static sipgo + pion + gobwas/ws + prometheus +
  zerolog OCI layer cannot plausibly fit under 1 MB even with maximum
  stripping — see `docs/operator/IMAGE_SIZE.md`.
- `DrainAll` now honours dual-leg sessions via `s.Terminate("shutdown")`
  (was `s.primary().dlg.Bye()` which only BYE'd `legs[0]`).
- HTTP shutdown ordering: stop-accept → DrainAll → UNREGISTER (was
  DrainAll → UNREGISTER → HTTP-stop).
- SIP listener address configurable via `SIP_LISTEN_ADDR` (was
  hardcoded `:5060`); enables co-existence with a co-hosted registrar
  stub for the e2e harness.
- Outbound INVITE port configurable via `SIP_OUTBOUND_TARGET_PORT`
  (defaults to DNS/SRV resolution; explicit override for the e2e
  harness UAS stubs).

### Deprecated

(none in v3.0; section header retained per Keep a Changelog v1.1.0
format.)

### Removed

- Pre-answer webhook fetch path (rejected during 2026-04-29 re-scope).
- `VOICE_URL`, `VOICE_METHOD`, `VOICE_FALLBACK_URL` env vars.
- `mode={shim,webhook}` field in `/health` JSON — locked to the 4-field
  shape.
- `internal/twiml/dispatcher.go` and `dispatcherAdapter`.

### Fixed

- `forward_duration_seconds{outcome}` cardinality regression —
  `recordFailure` now buckets unconditionally.
- Dual-leg drain leak — `DrainAndClose` + `ReleaseLegSequence` wired
  at terminal-dispatch so both legs of a forwarded call are released.
- `onInvite` now pre-registers the bridge session synchronously so
  the `CallSid` is mintable before the SIP 200 OK.
- Webhook subscription resolution unified across REST-supplied,
  TwiML-supplied, and operator-trusted default callbacks.
- Forwarder ring-timeout watchdog defends against a sipgo
  `WaitAnswer` path that does not honour `ctx.Done()` for INVITEs
  that never reach 180 Ringing — `DIAL_RING_TIMEOUT_S` is now
  enforced via an explicit `time.After` race regardless of remote
  provisional behaviour.

### Security

- REST 64 KB `MaxBytesReader` cap with Twilio-shaped 413 JSON body.
- Security headers on every REST response: `Strict-Transport-Security`
  (max-age=63072000), `Content-Security-Policy` (default-src 'none'),
  `X-Content-Type-Options: nosniff`.
- SSRF guard for status-callback URLs (RFC1918 / link-local / localhost
  rejected; bypass available only via the explicitly operator-trusted
  `STATUS_CALLBACK_DEFAULT_URL` env var).
- X-Twilio-Signature byte-fidelity (HMAC-SHA1; case-sensitive ASCII
  byte-sort; base64 std padding; raw URL fidelity).
- Toll-fraud default-deny posture (`DIAL_ALLOWED_PREFIXES` empty = no
  outbound calls; enforced at the SIP layer).
- Constant-time Basic Auth comparison (`subtle.ConstantTimeCompare`).
- Secret masking in logs (`AUTH_TOKEN` and `SIP_PASSWORD` masked with
  `***` via `NewSecretMaskWriter`). Note: env vars themselves are NOT
  masked — see `docs/operator/INCIDENT_RESPONSE.md`.

[Unreleased]: https://github.com/sipgate/audio-dock/compare/v3.1.0...HEAD
[3.1.0]: https://github.com/sipgate/audio-dock/compare/v3.0.0...v3.1.0
[3.0.0]: https://github.com/sipgate/audio-dock/releases/tag/v3.0.0
