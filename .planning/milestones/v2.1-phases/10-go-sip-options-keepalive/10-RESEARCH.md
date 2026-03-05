# Phase 10: Go SIP OPTIONS Keepalive - Research

**Researched:** 2026-03-05
**Domain:** Go SIP — out-of-dialog OPTIONS keepalive, goroutine lifecycle, sync.Mutex, Prometheus counter
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

#### Prometheus counter
- Add `sip_options_failures_total` counter to `go/internal/observability/metrics.go` in this phase (OBS-01 closes here)
- Increment on every failure (timeout, 5xx, 404) — not only when re-registration is triggered
- Follow existing pattern: counter on `observability.Metrics` struct, registered on custom registry

#### Log levels
- Successful OPTIONS ping: Debug — protocol noise, same reasoning as mark/clear in Phase 9
- OPTIONS failure (any failure that increments the counter): Warn — operationally significant
- Re-registration triggered by keepalive: Warn — log the trigger reason alongside the re-register attempt

#### Failure threshold
- 2 consecutive failures → trigger `doRegister` immediately
- Consecutive-failure counter resets to 0 on any successful OPTIONS response
- Counter variable lives inside `optionsKeepaliveLoop` — no field on `Registrar` struct needed

#### Re-registration during active calls
- Re-register immediately regardless of active calls — `doRegister` only refreshes the SIP binding; RTP is direct peer-to-peer and unaffected
- Mutex (already decided in STATE.md) only guards `doRegister` — OPTIONS request itself is out-of-dialog and stateless

#### 401/407 handling
- 401 or 407 response to OPTIONS = server is reachable, auth issue only → do NOT trigger re-registration
- Consecutive-failure counter is NOT incremented on 401/407

#### env var
- `SIPOptionsInterval time.Duration` with tag `env:"SIP_OPTIONS_INTERVAL" default:"30s"` added to `Config` struct
- Uses `go-simpler.org/env` duration parsing (same pattern as existing fields)

### Claude's Discretion

None specified — all key choices are locked above.

### Deferred Ideas (OUT OF SCOPE)

- In-dialog OPTIONS for session refresh (RFC 4028) — different feature, PROT-02 in REQUIREMENTS.md
- Sequence-number continuity for mark echoes — PROT-01, v2.x
- `SIP_OPTIONS_INTERVAL_S` integer env var variant — OBS-02 in REQUIREMENTS.md, already superseded by OPTS-05 implementation here
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| OPTS-01 | Go-Registrar sends SIP OPTIONS to sipgate every 30s | `time.NewTicker(cfg.SIPOptionsInterval)` in `optionsKeepaliveLoop`; same ticker pattern as `reregisterLoop` |
| OPTS-02 | On timeout, 5xx, or 404 response → immediate re-registration triggered | Response code switch in loop: `res == nil` (timeout/error) or `res.StatusCode >= 500` or `res.StatusCode == 404` → increment failure counter; on threshold 2, call `doRegister(ctx)` |
| OPTS-03 | On 401/407 response → no re-registration (server reachable, auth only) | Explicit `res.StatusCode == 401 \|\| res.StatusCode == 407` branch: reset failure counter, log Debug, continue loop without incrementing |
| OPTS-04 | OPTIONS keepalive goroutine is bound to root context and stops cleanly at SIGTERM | `go r.optionsKeepaliveLoop(ctx, cfg.SIPOptionsInterval)` in `Register()`; loop has `case <-ctx.Done(): return` as first select case |
| OPTS-05 | OPTIONS interval is configurable via `SIP_OPTIONS_INTERVAL` env var (default: 30s) | Add `SIPOptionsInterval time.Duration \`env:"SIP_OPTIONS_INTERVAL" default:"30s"\`` to `config.Config`; `go-simpler.org/env` v0.12.0 natively supports `time.Duration` |
</phase_requirements>

---

## Summary

Phase 10 adds an `optionsKeepaliveLoop` goroutine to the Go `Registrar`. The goroutine ticks every `SIPOptionsInterval` (default 30s), sends an out-of-dialog SIP OPTIONS request to the registrar host, and on 2 consecutive failures (timeout, 5xx, 404) calls `doRegister` to re-establish the SIP binding immediately. 401/407 responses do not count as failures — they confirm the server is alive. The goroutine is bound to the root context and stops on SIGTERM. A `sync.Mutex` on `doRegister` serializes concurrent calls from both `reregisterLoop` and `optionsKeepaliveLoop`. A new `sip_options_failures_total` Prometheus counter is added to `observability.Metrics`.

The implementation touches exactly 3 files: `go/internal/sip/registrar.go` (goroutine + mutex), `go/internal/config/config.go` (env var), `go/internal/observability/metrics.go` (counter). No new dependencies are required. The entire phase is modeled on patterns already in the codebase — `reregisterLoop` is a near-direct template for `optionsKeepaliveLoop`.

**Primary recommendation:** Implement in this order: (1) add `SIPOptionsInterval` to config + `SIPOptionsFailures` counter to metrics (compile-safe additions); (2) add `sync.Mutex` to `Registrar` + thread-guard `doRegister`; (3) implement and wire `optionsKeepaliveLoop`.

---

## Standard Stack

### Core (unchanged — no new dependencies)

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `github.com/emiago/sipgo` | v1.2.0 | `siplib.OPTIONS` method constant, `client.Do()` for out-of-dialog request | Already used for REGISTER; `siplib.OPTIONS RequestMethod = "OPTIONS"` confirmed in `sip/message.go:78` |
| `go-simpler.org/env` | v0.12.0 | Parses `SIP_OPTIONS_INTERVAL` as `time.Duration` with `default:"30s"` | `time.Duration` is listed in the supported types in `env.go:44`; `ParseDuration` semantics (e.g. "30s", "1m") |
| `github.com/prometheus/client_golang` | v1.23.2 | `prometheus.NewCounter` for `sip_options_failures_total` | Already used for all existing metrics; pattern is 2 lines to add a counter |
| `sync` | stdlib | `sync.Mutex` to serialize concurrent `doRegister` calls | No external library needed; standard Go mutex |
| `time` | stdlib | `time.NewTicker` for keepalive interval | Already used for `reregisterLoop` |
| `github.com/rs/zerolog` | v1.34.0 | Warn/Debug log calls in keepalive loop | Already used everywhere in registrar |

### No Alternatives Considered

Zero new dependencies. All required capabilities exist in the current stack.

**Installation:** No `go get` needed.

---

## Architecture Patterns

### Recommended File Changes

```
go/internal/
├── config/config.go          # Add SIPOptionsInterval time.Duration field
├── observability/metrics.go  # Add SIPOptionsFailures prometheus.Counter
└── sip/registrar.go          # Add sync.Mutex, mutex-guard doRegister, add optionsKeepaliveLoop
```

### Pattern 1: optionsKeepaliveLoop (direct template: reregisterLoop)

**What:** A goroutine that ticks at `SIPOptionsInterval`, sends OPTIONS, and drives re-registration on consecutive failures.

**When to use:** Exactly one instance; launched in `Register()` alongside `reregisterLoop`.

**Verified template from codebase** (`registrar.go:146-175` — `reregisterLoop`):
```go
// Source: go/internal/sip/registrar.go — reregisterLoop (direct model)
func (r *Registrar) reregisterLoop(ctx context.Context, expiry time.Duration) {
    retryIn := time.Duration(float64(expiry) * 0.75)
    ticker := time.NewTicker(retryIn)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // ... call doRegister
        }
    }
}
```

**optionsKeepaliveLoop follows the same skeleton:**
```go
func (r *Registrar) optionsKeepaliveLoop(ctx context.Context, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    consecutiveFailures := 0

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            res, err := r.sendOptions(ctx)
            if err != nil || isOptionsFailure(res) {
                consecutiveFailures++
                r.log.Warn().Int("consecutive_failures", consecutiveFailures).
                    Msg("OPTIONS keepalive: failure")
                if r.metrics != nil {
                    r.metrics.SIPOptionsFailures.Inc()
                }
                if consecutiveFailures >= 2 {
                    r.log.Warn().Msg("OPTIONS keepalive: threshold reached — triggering re-registration")
                    r.mu.Lock()
                    _, err := r.doRegister(ctx)
                    r.mu.Unlock()
                    if err != nil {
                        r.log.Error().Err(err).Msg("OPTIONS-triggered re-registration failed")
                        r.registered.Store(false)
                        if r.metrics != nil {
                            r.metrics.SIPRegStatus.Set(0)
                        }
                    }
                    consecutiveFailures = 0
                }
            } else if is401or407(res) {
                // Server alive — auth issue only. Do not increment failure counter.
                r.log.Debug().Int("status", res.StatusCode).
                    Msg("OPTIONS keepalive: 401/407 — server reachable, no re-registration")
                consecutiveFailures = 0
            } else {
                // Success (200 or other 2xx)
                r.log.Debug().Msg("OPTIONS keepalive: success")
                consecutiveFailures = 0
            }
        }
    }
}
```

### Pattern 2: sendOptions helper

**What:** Builds and sends the out-of-dialog OPTIONS request. Modeled on `doRegister` but minimal — no Expires, no Contact, no Auth retry.

**Key insight from doRegister** (`registrar.go:81-141`): From and To headers must be set explicitly before `client.Do()` because `ClientRequestBuild` derives From.User from the UA name (which is the binary name, not the SIP user). This same pitfall applies to OPTIONS.

```go
// Source: modeled on doRegister pattern — registrar.go lines 85-106
func (r *Registrar) sendOptions(ctx context.Context) (*siplib.Response, error) {
    registrarURI := siplib.Uri{Host: r.registrar, Port: 5060}
    req := siplib.NewRequest(siplib.OPTIONS, registrarURI)
    req.AppendHeader(siplib.NewHeader("User-Agent", "audio-dock/2.0"))
    req.AppendHeader(siplib.NewHeader("Max-Forwards", "70"))

    aor := r.aorURI()
    fromH := &siplib.FromHeader{Address: aor, Params: siplib.NewParams()}
    fromH.Params.Add("tag", siplib.GenerateTagN(16))
    req.AppendHeader(fromH)
    req.AppendHeader(&siplib.ToHeader{Address: aor})

    res, err := r.client.Do(ctx, req, sipgo.ClientRequestBuild)
    if err != nil {
        return nil, err
    }
    return res, nil
}
```

Note: `ClientRequestBuild` (not `ClientRequestRegisterBuild`) — no REGISTER-specific headers needed for OPTIONS.

### Pattern 3: sync.Mutex on doRegister

**What:** Add `mu sync.Mutex` to `Registrar` struct. Lock/unlock wraps each `doRegister(ctx)` call site (in `reregisterLoop` and in `optionsKeepaliveLoop`). The mutex is NOT held during the OPTIONS send itself — only during `doRegister`.

**Why mandatory:** `sipgo.Client.Do()` concurrency safety is undocumented (STATE.md confirmed concern, `client.go` has no mutex). Two concurrent REGISTER transactions against the same UA may produce undefined behavior in sipgo's transaction layer. Mutex serializes them.

```go
// Add to Registrar struct:
type Registrar struct {
    // ... existing fields ...
    mu sync.Mutex // serializes concurrent doRegister calls
}

// In reregisterLoop — wrap existing doRegister call:
r.mu.Lock()
newExpiry, err := r.doRegister(ctx)
r.mu.Unlock()

// In optionsKeepaliveLoop — wrap doRegister call:
r.mu.Lock()
_, err := r.doRegister(ctx)
r.mu.Unlock()
```

### Pattern 4: Config field addition

**What:** One line added to `config.Config` struct in `go/internal/config/config.go`.

```go
// Source: go/internal/config/config.go — existing field pattern
// Add after SIPExpires:
SIPOptionsInterval time.Duration `env:"SIP_OPTIONS_INTERVAL" default:"30s" usage:"Interval between SIP OPTIONS keepalive pings"`
```

`go-simpler.org/env` v0.12.0 explicitly supports `time.Duration` (confirmed: `env.go:44` lists `time.Duration` as supported type). The default `"30s"` is parsed via `time.ParseDuration`.

### Pattern 5: Prometheus counter addition

**What:** One new counter on `observability.Metrics`, following the exact pattern of the 7 existing counters.

```go
// Source: go/internal/observability/metrics.go — existing counter pattern
// Add to Metrics struct (after ClearReceived):
SIPOptionsFailures prometheus.Counter // sip_options_failures_total

// In NewMetrics() — after clearReceived creation:
sipOptionsFailures := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "sip_options_failures_total",
    Help: "Total SIP OPTIONS keepalive failures (timeout, 5xx, 404).",
})

// Add to reg.MustRegister(...):
reg.MustRegister(..., sipOptionsFailures)

// Add to return struct:
SIPOptionsFailures: sipOptionsFailures,
```

### Pattern 6: Wire optionsKeepaliveLoop in Register()

```go
// Source: go/internal/sip/registrar.go — Register() method lines 63-75
// After existing go r.reregisterLoop(ctx, expiry):
go r.reregisterLoop(ctx, expiry)
go r.optionsKeepaliveLoop(ctx, cfg.SIPOptionsInterval)
```

This requires passing `cfg` into `Register()` or storing `SIPOptionsInterval` on the `Registrar` struct during construction. Since `NewRegistrar` already receives the full `config.Config`, add `optionsInterval time.Duration` as a field on `Registrar` (initialized in `NewRegistrar`) — or pass `cfg.SIPOptionsInterval` directly.

**Recommended approach:** Store it on the struct (same as `expires int`):
```go
// In Registrar struct:
optionsInterval time.Duration

// In NewRegistrar:
optionsInterval: cfg.SIPOptionsInterval,

// In Register():
go r.optionsKeepaliveLoop(ctx, r.optionsInterval)
```

### Anti-Patterns to Avoid

- **Holding the mutex during OPTIONS send:** The mutex is for `doRegister` only. Holding it during `sendOptions` would block `reregisterLoop` for the entire OPTIONS round-trip (up to 30s if sipgate is slow), causing a re-registration timeout. Lock only around `doRegister`.
- **Incrementing failure counter on 401/407:** These responses prove the server is alive and responding. Only network-level failures (timeout, err != nil) and protocol failures (5xx, 404) indicate registration loss.
- **Resetting the consecutive-failure counter after calling doRegister:** Reset should happen regardless of whether doRegister succeeds or fails — the counter tracks OPTIONS connectivity, not REGISTER success. Reset unconditionally after reaching threshold (whether doRegister succeeds or not).
- **Nesting goroutines:** `optionsKeepaliveLoop` calls `doRegister` directly, not `Register`. `Register` starts goroutines; calling it from within a goroutine would spawn unbounded goroutine trees (Pitfall 6 documented in Phase 5 RESEARCH.md).
- **Using `ClientRequestRegisterBuild` for OPTIONS:** That option adds REGISTER-specific headers. Use `ClientRequestBuild` for OPTIONS.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Response-timeout detection | Custom deadline goroutine | `client.Do(ctx, req)` — context cancellation propagates | sipgo's `Do` already selects on `ctx.Done()`, `tx.Done()`, and `tx.Responses()`; timeout = return nil response with error |
| Concurrent doRegister serialization | Channel-based serialize queue | `sync.Mutex` on `doRegister` | Mutex is the idiomatic Go primitive for exclusive function access; channel serializer adds allocation and goroutine overhead for a non-hot path |
| Env var parsing for duration | `strconv.Atoi` + multiply | `go-simpler.org/env` default tag | Library already handles `time.Duration` natively; consistent with all other config fields |
| Failure counter persistence across restarts | Redis/file store | Local `int` in the loop | Counter resets on restart is acceptable — keepalive will detect any real failure within 2 intervals |

**Key insight:** This phase is almost entirely "plumbing" — connecting existing pieces. `reregisterLoop` already shows exactly how to build `optionsKeepaliveLoop`. The only genuinely new logic is the failure-counter state machine (8 lines).

---

## Common Pitfalls

### Pitfall 1: From/To Headers Missing on OPTIONS Request

**What goes wrong:** `client.Do(ctx, req, sipgo.ClientRequestBuild)` derives `From.User` from the sipgo UA name (the Go binary name, "audio-dock"), not from `r.user`. sipgate may reject or ignore an OPTIONS with the wrong AoR, or fail to correlate it as a legitimate keepalive from the registered identity.

**Why it happens:** `ClientRequestBuild` sets From only if the header is absent (confirmed: `client.go:338-353`). If we don't set From and To before calling `Do`, the builder fills From.User from `c.UserAgent.name`.

**How to avoid:** Explicitly set From (with tag) and To headers before calling `client.Do`, exactly as `doRegister` does (lines 94-98 of registrar.go).

**Warning signs:** sipgate returns 403 or drops OPTIONS; `client.Do` returns an error about unexpected response.

### Pitfall 2: Mutex Held During OPTIONS Send (Deadlock Risk)

**What goes wrong:** If `reregisterLoop` triggers at the same time as `optionsKeepaliveLoop` is holding the mutex during the OPTIONS round-trip, `reregisterLoop` blocks for the entire OPTIONS timeout. Since `reregisterLoop` is supposed to re-register at 75% of the Expires interval, a blocked loop can cause registration expiry.

**Why it happens:** Over-scoping the mutex lock to include `sendOptions` looks safe but creates a chokepoint at the wrong level.

**How to avoid:** Acquire the mutex only immediately before calling `doRegister`. `sendOptions` must run without the mutex.

**Warning signs:** Under load or sipgate latency, `reregisterLoop` logs show gaps exceeding `expiry * 0.75`.

### Pitfall 3: Consecutive-Failure Counter Not Reset on 401/407

**What goes wrong:** 401/407 is counted as a failure. After 2 such responses (which sipgate may send for any OPTIONS that arrives while digest auth has rotated), re-registration is incorrectly triggered even though the server is fully reachable.

**Why it happens:** A simple `if err != nil || res.StatusCode != 200` check incorrectly treats 401/407 as failure. The 401/407 handling requires an explicit guard.

**How to avoid:** Check for 401/407 BEFORE checking for general failure. Reset the counter and log at Debug level.

**Warning signs:** Spurious re-registration logs showing "threshold reached" even when the server is healthy.

### Pitfall 4: SIPOptionsInterval Field Not Stored on Registrar

**What goes wrong:** `Register(ctx)` currently has no access to `cfg.SIPOptionsInterval` unless it is stored during `NewRegistrar`. Trying to pass `cfg` into `Register(ctx)` changes the method signature (breaks callers in main.go).

**Why it happens:** `Register` only takes `ctx context.Context` today. Adding config here is a larger refactor than needed.

**How to avoid:** Store `optionsInterval time.Duration` on the `Registrar` struct in `NewRegistrar`, exactly as `expires int` is stored. `Register` reads `r.optionsInterval`.

**Warning signs:** Compile error "too many arguments to function Register" or "r.optionsInterval undefined".

### Pitfall 5: doRegister Not Protected by Mutex in reregisterLoop

**What goes wrong:** Adding the mutex to `optionsKeepaliveLoop` but forgetting to also add it to `reregisterLoop` means two concurrent REGISTER transactions can still be in flight.

**Why it happens:** `reregisterLoop` already calls `doRegister` and was written before the mutex decision. It must be updated in the same plan.

**How to avoid:** Update both call sites — `reregisterLoop` and `optionsKeepaliveLoop` — to use `r.mu.Lock() / r.mu.Unlock()` around `doRegister`.

**Warning signs:** `go test -race` reports data race on Registrar fields during concurrent `doRegister` calls.

### Pitfall 6: Goroutine Launched Before First Registration Succeeds

**What goes wrong:** `optionsKeepaliveLoop` launched before `doRegister` completes (e.g., in `NewRegistrar`) causes an OPTIONS to go out before the REGISTER binding exists. sipgate may return 404 for the AoR, triggering a re-registration loop before the initial registration is even done.

**Why it happens:** Launching keepalive too early.

**How to avoid:** Launch `go r.optionsKeepaliveLoop(ctx, r.optionsInterval)` inside `Register()` AFTER the initial `doRegister` call succeeds — same position as `reregisterLoop`. This is already the pattern established by `reregisterLoop`.

**Warning signs:** Log shows OPTIONS sent, 404 received, re-registration triggered — all before "SIP registration successful" log line.

---

## Code Examples

Verified patterns from source (HIGH confidence):

### siplib.OPTIONS constant
```go
// Source: /Users/rotmanov/go/pkg/mod/github.com/emiago/sipgo@v1.2.0/sip/message.go:78
OPTIONS RequestMethod = "OPTIONS"
// Usage:
req := siplib.NewRequest(siplib.OPTIONS, registrarURI)
```

### client.Do timeout behavior
```go
// Source: /Users/rotmanov/go/pkg/mod/github.com/emiago/sipgo@v1.2.0/client.go:213-233
// client.Do selects on ctx.Done() — network timeout = wrap ctx with timeout:
ctx30s, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
res, err := r.client.Do(ctx30s, req, sipgo.ClientRequestBuild)
// err != nil on timeout; res == nil on err
```

Note: A per-OPTIONS timeout (e.g. 10s) prevents the keepalive from being blocked for the full `SIPOptionsInterval` on a slow response.

### go-simpler/env duration support
```go
// Source: /Users/rotmanov/go/pkg/mod/go-simpler.org/env@v0.12.0/env.go:44
// Supported types include: time.Duration
// Config field — one line:
SIPOptionsInterval time.Duration `env:"SIP_OPTIONS_INTERVAL" default:"30s" usage:"Interval between SIP OPTIONS keepalive pings (e.g. 30s, 1m)"`
```

### Prometheus counter — existing pattern
```go
// Source: go/internal/observability/metrics.go:54-57 (clearReceived as template)
sipOptionsFailures := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "sip_options_failures_total",
    Help: "Total SIP OPTIONS keepalive failures (timeout, 5xx, 404).",
})
```

### Response code classification
```go
// Three-way classification for OPTIONS response:
func isOptionsFailure(res *siplib.Response, err error) bool {
    if err != nil {
        return true // network timeout or transport error
    }
    if res == nil {
        return true
    }
    switch {
    case res.StatusCode == 404:
        return true
    case res.StatusCode >= 500:
        return true
    default:
        return false
    }
}

func isOptionsAuth(res *siplib.Response) bool {
    return res != nil && (res.StatusCode == 401 || res.StatusCode == 407)
}
// success = !isOptionsFailure && !isOptionsAuth
```

### reregisterLoop mutex update (both call sites must be updated)
```go
// Source: go/internal/sip/registrar.go:156 — existing call site
// BEFORE (current code):
newExpiry, err := r.doRegister(ctx)

// AFTER (this phase):
r.mu.Lock()
newExpiry, err := r.doRegister(ctx)
r.mu.Unlock()
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Re-registration only at 75% expiry interval | Re-registration also triggered by keepalive failure | Phase 10 | Detect registration loss within 1 OPTIONS interval (30s) rather than at expiry (potentially 90s+ at 75% of 120s) |
| `doRegister` called from single goroutine | `doRegister` called from two concurrent goroutines | Phase 10 | Requires `sync.Mutex` — not optional |
| No liveness probe | SIP OPTIONS keepalive every 30s | Phase 10 | Operator visibility into trunk health via `sip_options_failures_total` counter |

**Deprecated/outdated:**
- None. The `reregisterLoop` approach is preserved as-is; OPTIONS keepalive is purely additive.

---

## Open Questions

1. **Per-OPTIONS context timeout value**
   - What we know: `client.Do` respects `ctx.Done()`. A per-request timeout prevents keepalive from stalling the entire interval window.
   - What's unclear: No official guidance on what timeout sipgate allows for unauthenticated OPTIONS. Typical SIP transaction timeout is T1*64 = ~32s, but for keepalive purposes a shorter timeout (e.g., 10s) is more useful.
   - Recommendation: Use `context.WithTimeout(ctx, 10*time.Second)` per OPTIONS send. This gives sipgate 10s to respond before the failure is counted. The 10s value can be hardcoded (not configurable) since the failure threshold of 2 already provides a buffer.

2. **sipgate behavior on out-of-dialog OPTIONS**
   - What we know: STATE.md documents this as an open concern — "Unknown if sipgate requires digest auth on out-of-dialog OPTIONS." The 401/407 handling in the response-code table covers this safely (401 = server alive).
   - What's unclear: Some SIP proxies silently drop out-of-dialog OPTIONS from unregistered sources.
   - Recommendation: Accept current design. If sipgate drops OPTIONS, `client.Do` will time out after 10s → counted as failure → re-registration triggered. The behavior is safe regardless of sipgate's policy.

3. **consecutiveFailures reset after doRegister**
   - What we know: CONTEXT.md says "counter resets to 0 on any successful OPTIONS response."
   - What's unclear: Should the counter also reset after a successful doRegister (triggered by threshold)? Or only on OPTIONS success?
   - Recommendation: Reset to 0 unconditionally after triggering doRegister (threshold reached), regardless of doRegister outcome. The next OPTIONS tick will re-evaluate fresh. This prevents a re-registration storm if doRegister keeps failing.

---

## Validation Architecture

`workflow.nyquist_validation` is not set to `true` in `.planning/config.json` — this section is skipped.

---

## Complete File Change Map

| File | Change Type | Description |
|------|-------------|-------------|
| `go/internal/config/config.go` | MODIFY | Add `SIPOptionsInterval time.Duration` field with env tag `SIP_OPTIONS_INTERVAL` default `30s` |
| `go/internal/observability/metrics.go` | MODIFY | Add `SIPOptionsFailures prometheus.Counter` field, create + register `sip_options_failures_total` counter |
| `go/internal/sip/registrar.go` | MODIFY | (1) Add `mu sync.Mutex` + `optionsInterval time.Duration` to `Registrar` struct; (2) Init `optionsInterval` in `NewRegistrar`; (3) Mutex-guard `doRegister` in `reregisterLoop`; (4) Add `sendOptions` helper; (5) Add `optionsKeepaliveLoop`; (6) Wire `go r.optionsKeepaliveLoop` in `Register()` |
| `go/internal/sip/registrar_test.go` | MODIFY | Add tests: threshold counter logic, 401/407 non-failure behavior, config field default |

No new files. No changes outside these 4 files. No new dependencies.

---

## Sources

### Primary (HIGH confidence)

- `go/internal/sip/registrar.go` (current source) — `reregisterLoop`, `doRegister`, `Registrar` struct, `Register()` wiring pattern; verified by direct file read
- `go/internal/config/config.go` (current source) — existing field pattern with `go-simpler.org/env` struct tags; verified by direct file read
- `go/internal/observability/metrics.go` (current source) — counter registration pattern; verified by direct file read
- `/Users/rotmanov/go/pkg/mod/github.com/emiago/sipgo@v1.2.0/sip/message.go:78` — `OPTIONS RequestMethod = "OPTIONS"` constant confirmed
- `/Users/rotmanov/go/pkg/mod/github.com/emiago/sipgo@v1.2.0/client.go:213-233` — `client.Do` signature, timeout behavior via `ctx.Done()`, no internal mutex
- `/Users/rotmanov/go/pkg/mod/github.com/emiago/sipgo@v1.2.0/client.go:328-418` — `ClientRequestBuild` behavior: adds From/To only if absent; From.User defaults to UA name if unset
- `/Users/rotmanov/go/pkg/mod/go-simpler.org/env@v0.12.0/env.go:44` — `time.Duration` listed as natively supported type
- `.planning/phases/10-go-sip-options-keepalive/10-CONTEXT.md` — all locked decisions

### Secondary (MEDIUM confidence)

- `.planning/STATE.md` — `doRegister mutex mandatory` decision, `sipgo concurrent client.Do() thread-safety undocumented` blocker
- Phase 9 RESEARCH.md (`09-RESEARCH.md`) — Prometheus counter pattern, goroutine lifecycle pattern references

### Tertiary (LOW confidence)

None — all findings backed by direct source code reads.

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — zero new dependencies; sipgo OPTIONS constant and client.Do verified from module cache
- Architecture: HIGH — goroutine model verified from registrar.go source; reregisterLoop is a direct template
- Pitfalls: HIGH — derived from direct code inspection (client.go mutex absence, ClientRequestBuild From.User behavior) and STATE.md documented concerns
- Env var: HIGH — go-simpler/env time.Duration support verified from module source

**Research date:** 2026-03-05
**Valid until:** 2026-06-05 (stable domain — sipgo v1.2.0 locked in go.mod; env library v0.12.0 locked; Go stdlib unchanged)
