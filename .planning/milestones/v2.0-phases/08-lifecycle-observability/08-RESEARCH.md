# Phase 8: Lifecycle + Observability - Research

**Researched:** 2026-03-04
**Domain:** Go graceful shutdown, Prometheus metrics exposition, net/http health endpoints
**Confidence:** HIGH

---

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| LCY-01 | On SIGTERM/SIGINT, service sends SIP BYE to all active calls, UNREGISTER, and closes all WebSocket connections before exiting | `signal.NotifyContext` already wired in main.go; `Registrar.Unregister()` already exists; `CallManager.sessions` sync.Map enables range-and-BYE drain; `shutdownFlag` atomic.Bool needed to reject new INVITEs during drain window; 10-second exit budget enforced with `context.WithTimeout` |
| OBS-02 | `GET /health` returns JSON with `registered: true/false` and `activeCalls: N` reflecting current state | `net/http` stdlib; `CallManager` needs `ActiveCount() int` method; `Registrar` needs `IsRegistered() bool` method backed by `atomic.Bool`; JSON encoding with `encoding/json` |
| OBS-03 | `GET /metrics` returns valid Prometheus exposition format including `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, `ws_reconnect_attempts_total` | `prometheus/client_golang v1.23.2`; custom `prometheus.NewRegistry()`; `promhttp.HandlerFor(reg, opts)`; counters incremented in rtpReader/rtpPacer/reconnect(); gauge for active calls and registration; `go get github.com/prometheus/client_golang/prometheus` |
</phase_requirements>

---

## Summary

Phase 8 adds three production-readiness features to an already fully functional audio bridge: graceful shutdown on SIGTERM, a `/health` JSON endpoint, and a `/metrics` Prometheus endpoint. All three are implemented using Go stdlib (`os/signal`, `net/http`, `sync/atomic`) plus one new dependency (`prometheus/client_golang`).

The **graceful shutdown** (LCY-01) builds on the existing `signal.NotifyContext` pattern already in `main.go`. The current code already calls `registrar.Unregister(ctx)` after `<-ctx.Done()` fires. What's missing is: (1) a `shutdownFlag` (`atomic.Bool`) that `onInvite` checks before answering new calls, (2) a drain loop in `CallManager` that ranges over `sessions` and calls `dlg.Bye()` on each active call, and (3) waiting for all sessions to finish (sessions self-delete from `sessions` sync.Map when their goroutine exits). The total shutdown budget is 10 seconds per the roadmap success criteria. The sequence is: set shutdownFlag → drain sessions (BYE all + wait) → Unregister → close UA.

The **observability layer** (OBS-02, OBS-03) runs as a separate `net/http` server on a configurable port (`HTTP_PORT`, defaulting to `8080`). For `/health`, `CallManager` exposes an `ActiveCount()` method and `Registrar` exposes an `IsRegistered() bool` (backed by an `atomic.Bool` toggled on Register/Unregister success). For `/metrics`, a custom `prometheus.Registry` holds five metrics: `active_calls_total` (Gauge), `sip_registration_status` (Gauge, 0/1), `rtp_packets_received_total` (Counter), `rtp_packets_sent_total` (Counter), `ws_reconnect_attempts_total` (Counter). The registry avoids the default global registry to prevent Go runtime/process metrics from appearing in the scrape output.

The HTTP server must be shut down gracefully alongside everything else: `httpServer.Shutdown(ctx)` is called as part of the Phase 8 shutdown sequence.

**Primary recommendation:** Implement as two plans — 08-01 for graceful shutdown (LCY-01) and 08-02 for HTTP server + health + metrics (OBS-02, OBS-03). No new dependencies for 08-01; one new dependency (`prometheus/client_golang`) for 08-02.

---

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `os/signal` + `syscall` | stdlib | `signal.NotifyContext` — already used in main.go | Already wired; `signal.NotifyContext` cancels the root context on SIGTERM/SIGINT |
| `sync/atomic` | stdlib | `atomic.Bool` for `shutdownFlag` and `isRegistered` | Zero-allocation, race-safe flag; idiomatic Go for simple boolean state |
| `net/http` | stdlib | HTTP server for /health and /metrics | No external dep; `http.Server.Shutdown(ctx)` for graceful drain |
| `encoding/json` | stdlib | JSON marshal for /health response | Already used throughout codebase |
| `github.com/prometheus/client_golang` | v1.23.2 | Prometheus metrics exposition | Industry standard; `promhttp.HandlerFor` produces valid Prometheus text format |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `context` stdlib | stdlib | `context.WithTimeout` for 10s shutdown budget | Wrap shutdown drain to prevent indefinite hang |
| `sync` stdlib | stdlib | `sync.WaitGroup` (or `WaitGroup.Go` — Go 1.25) for session drain | Already in use; Go 1.25 module adds `.Go()` convenience method |
| `time` stdlib | stdlib | Shutdown deadline: `context.WithTimeout(ctx, 10*time.Second)` | Enforces the 10s exit limit from ROADMAP success criteria |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Custom `prometheus.NewRegistry()` | `prometheus.DefaultRegisterer` | Default registry includes Go runtime/process metrics; custom registry exposes only sipgate-sip-stream-bridge metrics |
| `atomic.Bool` for `isRegistered` | Mutex-protected bool | `atomic.Bool` is simpler and zero-allocation; sufficient for a single flag |
| Separate HTTP server goroutine | Embedding in main goroutine | Separate goroutine allows `httpServer.Shutdown()` to be called from shutdown sequence cleanly |
| `promhttp.Handler()` (global registry) | `promhttp.HandlerFor(reg, opts)` | `HandlerFor` with a custom registry avoids polluting metrics with Go runtime stats |

**Installation:**
```bash
go get github.com/prometheus/client_golang/prometheus
go get github.com/prometheus/client_golang/prometheus/promhttp
```

---

## Architecture Patterns

### Recommended File Changes for Phase 8

```
cmd/sipgate-sip-stream-bridge/
└── main.go                     # MODIFY: shutdownFlag, HTTP server start, drain sequence

internal/bridge/
├── manager.go                  # MODIFY: add ActiveCount(), DrainAll(ctx), Metrics struct
└── session.go                  # MODIFY: increment rtp_packets_received/sent counters (optional — see note)

internal/sip/
├── registrar.go                # MODIFY: add IsRegistered() bool backed by atomic.Bool
└── handler.go                  # MODIFY: check shutdownFlag in onInvite, reject with 503 if set

internal/observability/         # NEW PACKAGE (or place in bridge/manager.go)
└── metrics.go                  # Prometheus registry + 5 metric definitions
```

**Note on package placement:** Metrics can live in a new `internal/observability` package or as a `Metrics` struct in `internal/bridge`. The new-package approach avoids importing prometheus into existing bridge files. Either is valid; the planner should choose the simpler option.

### Pattern 1: Graceful Shutdown Sequence in main.go

**What:** After `<-ctx.Done()` fires (signal received), execute BYE drain → Unregister → HTTP shutdown → process exit in a timed context.

**When to use:** Always on SIGTERM/SIGINT (LCY-01).

```go
// Source: Go stdlib os/signal, context patterns; existing main.go structure
// Existing: <-ctx.Done() already fires on SIGTERM/SIGINT via signal.NotifyContext

logger.Info().Msg("shutdown signal received — starting graceful drain")

// 1. Signal new INVITEs to be rejected (shutdownFlag set before drain)
handler.SetShutdown() // sets atomic.Bool in Handler

// 2. Drain all active call sessions: BYE each, wait for goroutines to exit
// 10-second budget per ROADMAP success criteria
drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Second)
defer drainCancel()
callManager.DrainAll(drainCtx) // sends BYE to each session; waits for sessions.Delete

// 3. SIP UNREGISTER (already in existing main.go; keep with 5s timeout)
unregCtx, unregCancel := context.WithTimeout(context.Background(), 5*time.Second)
defer unregCancel()
if err := registrar.Unregister(unregCtx); err != nil {
    logger.Warn().Err(err).Msg("UNREGISTER failed during shutdown")
} else {
    logger.Info().Msg("SIP unregistered")
}

// 4. HTTP server graceful drain
httpShutCtx, httpShutCancel := context.WithTimeout(context.Background(), 2*time.Second)
defer httpShutCancel()
_ = httpServer.Shutdown(httpShutCtx)

logger.Info().Msg("shutdown complete")
// process exits naturally (main returns)
```

**Total budget note:** The 10-second exit requirement from the ROADMAP is a ceiling, not a floor. BYE drain gets 8s, UNREGISTER gets 5s (overlapping if needed), HTTP gets 2s — the dominant path is BYE drain. These contexts are independent and can run sequentially.

### Pattern 2: shutdownFlag in Handler (Reject New INVITEs During Drain)

**What:** Add an `atomic.Bool` to `Handler`. Set it before the drain loop. `onInvite` checks it at the top and responds 503 if set.

```go
// Source: Go stdlib sync/atomic; existing handler.go onInvite structure

// In Handler struct:
type Handler struct {
    // ... existing fields ...
    shutdown atomic.Bool // set on graceful shutdown; onInvite checks this
}

// SetShutdown is called from main.go after <-ctx.Done()
func (h *Handler) SetShutdown() {
    h.shutdown.Store(true)
}

// In onInvite, add at the top (before ReadInvite):
func (h *Handler) onInvite(req *siplib.Request, tx siplib.ServerTransaction) {
    if h.shutdown.Load() {
        _ = tx.Respond(siplib.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
        return
    }
    // ... rest of existing onInvite ...
}
```

**Why 503 not 480:** RFC 3261 §21.5.4: 503 Service Unavailable is the correct response when the server is shutting down and cannot process new requests. 480 Temporarily Unavailable implies a resource is absent, not that the service is draining.

### Pattern 3: CallManager.DrainAll — BYE Each Active Session

**What:** Range over `sessions` sync.Map, call `dlg.Bye()` on each. Use a separate WaitGroup to detect when all sessions have self-deleted.

**Critical insight:** `CallSession.run()` calls `m.sessions.Delete(callID)` via `defer m.sessions.Delete(callID)` inside `StartSession`. After `dlg.Bye()` is called, `sessionCtx.Done()` fires inside `run()`, causing the goroutine to drain and call `sessions.Delete`. Therefore, DrainAll does NOT need direct access to CallSession goroutines — it just needs to BYE each dialog and wait until `sessions` is empty.

```go
// Source: Go stdlib sync, context; existing CallManager sync.Map pattern

// DrainAll sends BYE to every active call and waits until all sessions have exited.
// Uses polling with deadline rather than a WaitGroup because sessions self-delete via
// the existing 'defer m.sessions.Delete(callID)' in StartSession.
func (m *CallManager) DrainAll(ctx context.Context) {
    // 1. BYE every active session
    m.sessions.Range(func(key, value any) bool {
        s := value.(*CallSession)
        m.log.Info().Str("call_id", s.callID).Msg("shutdown: sending BYE to active call")
        _ = s.dlg.Bye(context.Background())
        return true
    })

    // 2. Wait until sessions map is empty (sessions self-delete on exit)
    ticker := time.NewTicker(50 * time.Millisecond)
    defer ticker.Stop()
    for {
        count := 0
        m.sessions.Range(func(_, _ any) bool { count++; return true })
        if count == 0 {
            return
        }
        select {
        case <-ctx.Done():
            m.log.Warn().Int("remaining", count).Msg("shutdown: drain timeout — abandoning active sessions")
            return
        case <-ticker.C:
        }
    }
}

// ActiveCount returns the number of currently active call sessions.
func (m *CallManager) ActiveCount() int {
    count := 0
    m.sessions.Range(func(_, _ any) bool { count++; return true })
    return count
}
```

**Alternative drain approach:** Add a `drainWg sync.WaitGroup` to CallManager, and call `drainWg.Add(1)` in StartSession + `drainWg.Done()` at exit. Then `DrainAll` calls BYE on all, then `drainWg.Wait()` with a timeout. This is more precise but adds state to CallManager. The polling approach is simpler and correct for low call counts.

### Pattern 4: Registrar.IsRegistered() for /health

**What:** Add an `atomic.Bool` to `Registrar`. Set it `true` on successful register, `false` on failed re-register or Unregister.

```go
// Source: Go stdlib sync/atomic

// In Registrar struct:
type Registrar struct {
    // ... existing fields ...
    registered atomic.Bool
}

// IsRegistered returns the current registration state.
func (r *Registrar) IsRegistered() bool {
    return r.registered.Load()
}

// In doRegister, on success (after res.StatusCode == 200 check):
r.registered.Store(true)

// In Unregister, on success:
r.registered.Store(false)

// In reregisterLoop, on error path (to reflect degraded state):
r.registered.Store(false) // or keep true — depends on desired behavior; see Open Questions
```

### Pattern 5: /health HTTP Endpoint

**What:** Inline handler in `net/http` serving JSON `{"registered":bool,"activeCalls":int}`.

```go
// Source: Go stdlib net/http, encoding/json

mux := http.NewServeMux()

mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
    type healthResponse struct {
        Registered  bool `json:"registered"`
        ActiveCalls int  `json:"activeCalls"`
    }
    resp := healthResponse{
        Registered:  registrar.IsRegistered(),
        ActiveCalls: callManager.ActiveCount(),
    }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK) // always 200 — let scraper decide on values
    _ = json.NewEncoder(w).Encode(resp)
})
```

**Note on response code:** Return 200 regardless of registration state. The health endpoint reflects state for monitoring; returning 503 when `registered=false` would cause load balancers to yank the instance during normal re-register windows. Callers should inspect the JSON body, not the HTTP status code.

### Pattern 6: /metrics Endpoint with Custom Registry

**What:** Custom `prometheus.Registry` containing exactly the five required metrics. `promhttp.HandlerFor` serves the Prometheus text exposition format.

```go
// Source: pkg.go.dev/github.com/prometheus/client_golang v1.23.2
// Source: https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp

reg := prometheus.NewRegistry()

activeCallsGauge := prometheus.NewGauge(prometheus.GaugeOpts{
    Name: "active_calls_total",
    Help: "Number of currently active SIP call sessions.",
})

sipRegGauge := prometheus.NewGauge(prometheus.GaugeOpts{
    Name: "sip_registration_status",
    Help: "SIP registration status: 1 = registered, 0 = unregistered/failed.",
})

rtpRxCounter := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "rtp_packets_received_total",
    Help: "Total RTP packets received from the SIP caller.",
})

rtpTxCounter := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "rtp_packets_sent_total",
    Help: "Total RTP packets sent to the SIP caller.",
})

wsReconnectCounter := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "ws_reconnect_attempts_total",
    Help: "Total WebSocket reconnect attempts across all calls.",
})

reg.MustRegister(
    activeCallsGauge,
    sipRegGauge,
    rtpRxCounter,
    rtpTxCounter,
    wsReconnectCounter,
)

mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
```

**Metric update pattern:** Counters (`rtp_packets_received_total`, `rtp_packets_sent_total`, `ws_reconnect_attempts_total`) are incremented via counter references passed into `CallSession` at construction. The gauge `active_calls_total` is updated by `CallManager` when sessions start/end. `sip_registration_status` is updated by `Registrar` alongside the `registered atomic.Bool`.

**Passing metrics to CallSession:** A `Metrics` struct (or individual counter references) is passed via `CallManager` → `CallSession`. `rtpReader` calls `metrics.RTPRxCounter.Inc()` on each valid RTP packet. `rtpPacer` calls `metrics.RTPTxCounter.Inc()` on each sent packet. `reconnect()` calls `metrics.WSReconnectCounter.Inc()` on each attempt.

### Pattern 7: HTTP Server Lifecycle in main.go

```go
// Source: Go stdlib net/http; standard graceful shutdown pattern

httpPort := cfg.HTTPPort // e.g. "8080" — new optional config field
httpServer := &http.Server{
    Addr:    ":" + httpPort,
    Handler: mux,
}

go func() {
    if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        logger.Error().Err(err).Msg("HTTP server error")
    }
}()

// ... <-ctx.Done() waits for signal ...

// During shutdown sequence:
httpShutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()
_ = httpServer.Shutdown(httpShutCtx)
```

### Anti-Patterns to Avoid

- **Calling Unregister before DrainAll completes:** Unregistering while calls are still active is valid per RFC 3261 but confuses sipgate's routing — active calls may lose their route. Always BYE-drain first, then Unregister.
- **Closing `agent.UA` before DrainAll:** `ua.Close()` terminates the SIP transport, which can prevent BYE responses from being received. Keep UA alive through the entire drain.
- **Using the default prometheus registry:** `prometheus.DefaultRegisterer` pre-registers Go runtime metrics (goroutines, GC, etc.). Using a custom `NewRegistry()` gives a clean scrape output with only sipgate-sip-stream-bridge metrics.
- **Storing `*CallSession` in sync.Map but exposing it for external mutation:** `DrainAll` should call `dlg.Bye()` only; it should not cancel the session context directly (that is the session's responsibility via `dlg.Context()` cancellation).
- **Registering metrics inside CallSession:** Metrics are registered once at startup. CallSession receives counter references and calls `.Inc()` — it never registers or unregisters metrics.

---

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Prometheus text exposition format | Custom `/metrics` text writer | `promhttp.HandlerFor(reg, opts)` | The Prometheus text format has edge cases (float NaN, histogram buckets, labels escaping); promhttp handles all of it correctly |
| HTTP server graceful drain | Manual connection tracking | `httpServer.Shutdown(ctx)` | stdlib handles in-flight request draining, connection close, and deadline; correct behavior under concurrent requests |
| Atomic registration flag | `sync.Mutex` + bool | `atomic.Bool` | Zero-allocation, no lock contention; correct for a single-value flag with no compound operation |
| Poll-and-wait on sessions drain | Channel-based notification | Polling `sessions.Range` every 50ms | The sessions already self-delete via existing `defer m.sessions.Delete`; adding a drain channel requires modifying the session close path; polling is simpler and sufficient |

**Key insight:** The Prometheus client library handles one genuinely hard problem: the exposition format requires specific Content-Type negotiation (text vs. OpenMetrics) and correct float formatting for all metric values. `promhttp.HandlerFor` handles this automatically with zero configuration.

---

## Common Pitfalls

### Pitfall 1: Shutdown Races — UA Closed Before BYE Sent

**What goes wrong:** `agent.UA.Close()` is called (in existing `defer agent.UA.Close()` in main.go) while `DrainAll` is still sending BYEs. The SIP transport is torn down before BYE responses arrive.

**Why it happens:** `defer agent.UA.Close()` in the current main.go runs after `main()` returns, which happens naturally. But if the drain sequence returns early (timeout), the deferred `UA.Close()` runs correctly after. The risk is explicit `UA.Close()` calls inserted during Phase 8 that run before draining.

**How to avoid:** Do NOT add an explicit `UA.Close()` call to the shutdown sequence. The existing `defer agent.UA.Close()` at the top of `main()` is sufficient and runs last. Let the drain sequence complete, then the deferred close runs.

**Warning signs:** BYE sent but no 200 OK logged; remote peer left in CONFIRMED dialog state.

### Pitfall 2: sync.Map Concurrent Iteration + Deletion

**What goes wrong:** `DrainAll` ranges over `sessions` while calling `dlg.Bye()`, which eventually causes sessions to call `sessions.Delete(callID)` from their own goroutines. `sync.Map.Range` is documented to tolerate concurrent modification — deletions during range are safe but newly added entries may or may not be visited.

**Why it happens:** New calls can arrive during the tiny window between `handler.SetShutdown()` and `sessions.Range` starting.

**How to avoid:** Set `shutdownFlag` BEFORE calling `DrainAll`. The `onInvite` shutdownFlag check (Pattern 2) prevents new sessions from being added. This eliminates the race between drain and new session registration.

**Warning signs:** Calls answered after SIGTERM received; session count doesn't reach zero after drain.

### Pitfall 3: Double-Close on UA from Existing Defer + Phase 8 Explicit Close

**What goes wrong:** Phase 8 adds `agent.UA.Close()` to the shutdown sequence explicitly, but the existing `defer agent.UA.Close()` in main.go is still there. Two `UA.Close()` calls. In sipgo v1.2.0, `Server.Close()` returns nil (no-op), but `UA.Close()` may have side effects.

**Why it happens:** Adding shutdown steps without removing or consolidating existing ones.

**How to avoid:** Remove the `defer agent.UA.Close()` if adding an explicit close, or rely solely on the defer (preferred). Do not add an explicit close.

**Warning signs:** Panic or nil-pointer in sipgo internals during shutdown.

### Pitfall 4: Prometheus Metric Name Collision with Default Registry

**What goes wrong:** If `prometheus.DefaultRegisterer` is accidentally used (e.g., calling `prometheus.MustRegister` instead of `reg.MustRegister`), the metric is registered in the global registry. The `promhttp.HandlerFor(reg, opts)` handler only serves the custom registry — the metric appears registered but never appears in scrape output.

**Why it happens:** `prometheus.MustRegister(c)` (global) vs `reg.MustRegister(c)` (custom) are easy to confuse.

**How to avoid:** Always use `reg.MustRegister(c)` where `reg` is the custom registry returned by `prometheus.NewRegistry()`. Never call the package-level `prometheus.MustRegister`.

**Warning signs:** `go test` panics about "duplicate metrics registration"; scrape output missing expected metrics.

### Pitfall 5: isRegistered Transitions During Re-Register Window

**What goes wrong:** `isRegistered` is set `false` at re-register attempt start and only set `true` on success. The `/health` endpoint returns `registered: false` during the ~100ms re-register round-trip, causing false alerting.

**Why it happens:** Eagerly clearing the flag before the new REGISTER completes.

**How to avoid:** Set `isRegistered = false` ONLY on explicit failure (403, network error) and on `Unregister`. Do NOT clear it at the start of each `doRegister` attempt. The current `reregisterLoop` calls `doRegister` → checks error → logs. Add `r.registered.Store(false)` only on the `continue` (error) path, not before the call.

**Warning signs:** Prometheus `sip_registration_status` flickers 0→1 every 90 seconds in sync with re-register intervals.

### Pitfall 6: HTTP Server Port Collision

**What goes wrong:** `HTTP_PORT` defaults to 8080 but `docker-compose.yml` or the container runtime already uses that port.

**Why it happens:** 8080 is a common default.

**How to avoid:** Make `HTTP_PORT` an optional config field (not required). Default to `:8080`. Document the env var in docker-compose.yml. Test in Docker with `--network host` (already the production mode) — on Linux `network_mode: host` means port 8080 is exposed directly.

**Warning signs:** `bind: address already in use` at startup.

### Pitfall 7: Metrics Not Updated When Call Ends or Reconnect Fires

**What goes wrong:** `active_calls_total` gauge never decrements; `ws_reconnect_attempts_total` never increments. Metrics are stale.

**Why it happens:** Metrics are registered at startup but the update calls are not wired into the code paths that change state.

**How to avoid:** Update metric values at the following specific points:
- `active_calls_total.Inc()` — in `StartSession`, after `m.sessions.Store`
- `active_calls_total.Dec()` — in `StartSession`, in the `defer m.sessions.Delete` block
- `sip_registration_status.Set(1)` — in `doRegister`, on `res.StatusCode == 200`
- `sip_registration_status.Set(0)` — in `Unregister` on success, and on `doRegister` failure
- `rtp_packets_received_total.Inc()` — in `rtpReader`, for each valid PCMU packet processed
- `rtp_packets_sent_total.Inc()` — in `rtpPacer`, for each packet sent to caller
- `ws_reconnect_attempts_total.Inc()` — in `reconnect()`, at the top of each attempt loop iteration

**Warning signs:** Prometheus scrape shows metric values stuck at 0 or initial values; `active_calls_total` only ever increases.

---

## Code Examples

Verified patterns from official sources:

### Custom Prometheus Registry with HandlerFor

```go
// Source: pkg.go.dev/github.com/prometheus/client_golang/prometheus v1.23.2
// Source: pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp v1.23.2

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

// Create isolated registry — no default Go runtime metrics
reg := prometheus.NewRegistry()

activeCallsGauge := prometheus.NewGauge(prometheus.GaugeOpts{
    Name: "active_calls_total",
    Help: "Number of currently active SIP call sessions.",
})
reg.MustRegister(activeCallsGauge)

// Serve — HandlerFor uses custom registry, not DefaultGatherer
mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
```

### net/http Server Graceful Shutdown

```go
// Source: Go stdlib net/http — https://pkg.go.dev/net/http#Server.Shutdown
// "Shutdown gracefully shuts down the server without interrupting any active connections."

srv := &http.Server{Addr: ":8080", Handler: mux}

go func() {
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        logger.Error().Err(err).Msg("HTTP server error")
    }
}()

// On shutdown:
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()
if err := srv.Shutdown(ctx); err != nil {
    logger.Warn().Err(err).Msg("HTTP server shutdown error")
}
```

### atomic.Bool Registration Flag

```go
// Source: Go stdlib sync/atomic — https://pkg.go.dev/sync/atomic#Bool
// Available since Go 1.19; zero value is false.

var registered atomic.Bool
registered.Store(true)  // on successful REGISTER
registered.Store(false) // on UNREGISTER or registration failure
val := registered.Load() // thread-safe read
```

### WaitGroup.Go (Go 1.25 — module already on go 1.25.0)

```go
// Source: https://pkg.go.dev/sync#WaitGroup.Go
// Available since Go 1.25; go.mod already specifies go 1.25.0

var wg sync.WaitGroup
wg.Go(func() { someWork() }) // equivalent to wg.Add(1); go func() { defer wg.Done(); someWork() }()
wg.Wait()
```

### /health Response JSON

```go
// Source: encoding/json stdlib; requirement OBS-02

type HealthResponse struct {
    Registered  bool `json:"registered"`
    ActiveCalls int  `json:"activeCalls"`
}

w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusOK)
_ = json.NewEncoder(w).Encode(HealthResponse{
    Registered:  registrar.IsRegistered(),
    ActiveCalls: callManager.ActiveCount(),
})
```

---

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `<-ctx.Done()` → immediate exit | `<-ctx.Done()` → drain BYE → Unregister → exit | Phase 8 | LCY-01 satisfied; peers know the session is over |
| No /health endpoint | `GET /health` JSON | Phase 8 | OBS-02 satisfied; container orchestrators can health-check |
| No /metrics endpoint | `GET /metrics` Prometheus | Phase 8 | OBS-03 satisfied; Prometheus scrape enabled |
| No shutdownFlag in INVITE handler | `atomic.Bool` checked at top of `onInvite` | Phase 8 | New calls rejected during drain window |
| Partial shutdown (Unregister only) in main.go | Full drain: BYE all → Unregister → HTTP close | Phase 8 | Peers not left in unknown state |

---

## Open Questions

1. **Should `isRegistered` flip to `false` during a re-register attempt?**
   - What we know: sipgate grants short Expires (~120s) and re-register fires at 75% (~90s). Typically completes in <200ms.
   - Recommendation: Do NOT flip false during the attempt. Only flip false on explicit failure. This avoids a 200ms `/health` false-negative every 90 seconds.

2. **Should `active_calls_total` be a Gauge (goes up/down) or a Counter (monotonically increasing)?**
   - What we know: OBS-03 uses the name `active_calls_total` — the `_total` suffix is Prometheus convention for Counters. But the value tracks current active sessions (a Gauge concept). The requirement uses `active_calls_total` literally.
   - Recommendation: Use a `Gauge` named `active_calls_total` as specified by OBS-03. The `_total` suffix is conventional for Counters but not enforced. Document the deviation in code comments. If strict Prometheus naming is desired, rename to `active_calls` (no `_total`) — but only if the requirement is OK with renaming.

3. **Where should the Metrics struct live?**
   - What we know: `CallSession.rtpReader` and `rtpPacer` need counter references. `CallManager` needs gauge references. `Registrar` needs the `sip_registration_status` gauge reference.
   - Recommendation: Create an `internal/observability` package with a `Metrics` struct. Pass `*observability.Metrics` to `CallManager` and `Registrar` at construction. This avoids importing prometheus into existing packages.

4. **HTTP_PORT config field — required or optional?**
   - What we know: All existing config fields are either required (SIP creds, WS URL) or optional with defaults (RTP ports, expires). An HTTP port is clearly optional.
   - Recommendation: Add `HTTPPort string \`env:"HTTP_PORT" default:"8080"\`` to `config.Config`. This follows the existing `go-simpler/env` pattern.

---

## Sources

### Primary (HIGH confidence)

- `pkg.go.dev/github.com/prometheus/client_golang/prometheus` v1.23.2 — Counter, Gauge, NewRegistry, MustRegister APIs verified 2026-03-04
- `pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp` v1.23.2 — HandlerFor, HandlerOpts verified 2026-03-04
- Go stdlib `pkg.go.dev/net/http#Server.Shutdown` — graceful drain API verified; `http.ErrServerClosed` sentinel verified
- Go stdlib `pkg.go.dev/sync/atomic#Bool` — atomic.Bool zero-value semantics verified
- Go stdlib `pkg.go.dev/sync#WaitGroup.Go` — WaitGroup.Go added in Go 1.25 (go.mod already specifies go 1.25.0) verified
- `internal/bridge/manager.go` (current) — `sessions sync.Map`, `defer m.sessions.Delete(callID)` in StartSession confirmed; BYE-by-range pattern is sound
- `internal/sip/registrar.go` (current) — `Unregister()` already exists and handles Digest Auth; `doRegister()` structure confirmed for flag insertion points
- `internal/sip/handler.go` (current) — `onInvite` structure confirmed; shutdownFlag check location identified
- `cmd/sipgate-sip-stream-bridge/main.go` (current) — `signal.NotifyContext` already wired; `registrar.Unregister` already called; `defer agent.UA.Close()` confirmed
- `github.com/prometheus/client_golang` releases page — v1.23.2 confirmed as latest stable (2025-09-05)

### Secondary (MEDIUM confidence)

- ROADMAP.md Phase 8 success criteria — "exits within 10 seconds" confirmed; /health and /metrics response schemas confirmed
- REQUIREMENTS.md OBS-03 — exact metric names confirmed: `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, `ws_reconnect_attempts_total`
- Prometheus naming conventions — `_total` suffix for Counters is convention, not enforcement; Gauge named `active_calls_total` is valid

### Tertiary (LOW confidence — validate at implementation)

- sipgate behavior with Expires: 0 (UNREGISTER) immediately after calls are BYEd — likely clean but not tested against live sipgate
- `sync.Map.Range` behavior when entries are deleted by concurrent goroutines during range — documented as safe but exact visit ordering not guaranteed; mitigated by shutdownFlag

---

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — prometheus/client_golang v1.23.2 verified from pkg.go.dev; all stdlib APIs verified
- Architecture: HIGH — derived directly from reading existing main.go, manager.go, session.go, registrar.go, handler.go; no guesswork
- Shutdown sequence: HIGH — existing structure in main.go is already partially correct; Phase 8 adds BYE drain and shutdownFlag only
- Prometheus metrics: HIGH — pkg.go.dev verified; metric names from REQUIREMENTS.md OBS-03
- Pitfalls: HIGH — derived from reading existing code paths and Go concurrency fundamentals; UA double-close risk directly observable in main.go

**Research date:** 2026-03-04
**Valid until:** 2026-09-04 (prometheus/client_golang is stable; net/http stdlib is stable; recheck if sipgo v1.x introduces built-in graceful shutdown)
