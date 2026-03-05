# Phase 10: Go SIP OPTIONS Keepalive - Context

**Gathered:** 2026-03-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Add an `optionsKeepaliveLoop` goroutine to the Go `Registrar` that sends a periodic out-of-dialog SIP OPTIONS request to sipgate. When the ping fails (timeout/5xx/404), increment the consecutive-failure counter; on reaching threshold (2), call `doRegister` to re-register immediately. Scope: `go/internal/sip/registrar.go` and `go/internal/config/config.go`. Node.js (Phase 11) is out of scope.

</domain>

<decisions>
## Implementation Decisions

### Prometheus counter
- Add `sip_options_failures_total` counter to `go/internal/observability/metrics.go` **in this phase** (OBS-01 closes here)
- Increment on **every failure** (timeout, 5xx, 404) — not only when re-registration is triggered
- Follow existing pattern: counter on `observability.Metrics` struct, registered on custom registry

### Log levels
- Successful OPTIONS ping: **Debug** — protocol noise, same reasoning as mark/clear in Phase 9
- OPTIONS failure (any failure that increments the counter): **Warn** — operationally significant; operator should see it, but it's not fatal since re-registration may succeed
- Re-registration triggered by keepalive: **Warn** — log the trigger reason alongside the re-register attempt

### Failure threshold
- **2 consecutive failures** → trigger `doRegister` immediately
- Consecutive-failure counter **resets to 0 on any successful OPTIONS response**
- Counter variable lives inside `optionsKeepaliveLoop` — no field on `Registrar` struct needed

### Re-registration during active calls
- **Re-register immediately regardless of active calls** — `doRegister` only refreshes the SIP binding at sipgate; RTP is direct peer-to-peer and unaffected. REGISTER and in-dialog RTP are independent per RFC 3261.
- Mutex (already decided in STATE.md) only guards `doRegister` — OPTIONS request itself is out-of-dialog and stateless, so OPTIONS and REGISTER can be in-flight simultaneously

### 401/407 handling (carried forward from STATE.md)
- 401 or 407 response to OPTIONS = server is reachable, auth issue only → **do NOT trigger re-registration**
- Consecutive-failure counter is NOT incremented on 401/407 (server alive)

### env var
- `SIPOptionsInterval time.Duration` with tag `env:"SIP_OPTIONS_INTERVAL" default:"30s"` added to `Config` struct in `go/internal/config/config.go`
- Uses `go-simpler.org/env` duration parsing (same pattern as existing fields)

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- `reregisterLoop` in `registrar.go` — direct model for `optionsKeepaliveLoop`: same `time.NewTicker`, same `select { case <-ctx.Done(): return; case <-ticker.C: }` pattern
- `doRegister(ctx)` — call this on threshold reached; already handles Digest Auth, Expires extraction, `registered.Store(true)`, metrics update
- `r.client.Do(ctx, req, ...)` — existing pattern for sending out-of-dialog SIP requests (REGISTER uses this; OPTIONS uses same path)
- `go-simpler.org/env` struct tags — adding `SIPOptionsInterval time.Duration \`env:"SIP_OPTIONS_INTERVAL" default:"30s"\`` is one line in `config.go`

### Established Patterns
- **sync.Mutex on doRegister** (STATE.md locked decision) — `optionsKeepaliveLoop` and `reregisterLoop` may both call `doRegister` concurrently; mutex serializes them. Mutex is NOT held during the OPTIONS request itself.
- **Goroutine bound to root context** — `go r.optionsKeepaliveLoop(ctx, interval)` in `Register()`, same as `go r.reregisterLoop(ctx, expiry)`. Loop returns when `ctx.Done()`.
- **Non-fatal re-registration failure** — same `continue` behaviour as `reregisterLoop`: log error, set `registered.Store(false)`, keep ticker running for next tick

### Integration Points
- `Registrar.Register(ctx)` — add `go r.optionsKeepaliveLoop(ctx, cfg.SIPOptionsInterval)` after `go r.reregisterLoop(ctx, expiry)`
- `config.Config` struct — add `SIPOptionsInterval time.Duration` field (one line)
- `observability.Metrics` struct + `NewMetrics()` — add `SIPOptionsFailures prometheus.Counter` field and register `sip_options_failures_total`
- `registrar.go` imports — no new dependencies; `siplib.OPTIONS` method constant already in emiago/sipgo/sip

</code_context>

<specifics>
## Specific Ideas

- OPTIONS request URI: `sip:r.registrar:5060` (same as REGISTER Request-URI) — out-of-dialog OPTIONS go to the registrar host
- OPTIONS method constant in sipgo: `siplib.OPTIONS` — same package as `siplib.REGISTER`
- No Contact or Expires headers needed on OPTIONS — minimal request: From, To, Max-Forwards only
- Consecutive failure counter is a plain `int` local variable inside the loop, reset on success

</specifics>

<deferred>
## Deferred Ideas

- In-dialog OPTIONS for session refresh (RFC 4028) — different feature, PROT-02 in REQUIREMENTS.md
- Sequence-number continuity for mark echoes — PROT-01, v2.x
- `SIP_OPTIONS_INTERVAL_S` integer env var variant — OBS-02 in REQUIREMENTS.md, already superseded by OPTS-05 implementation here

</deferred>

---

*Phase: 10-go-sip-options-keepalive*
*Context gathered: 2026-03-05*
