---
phase: "08"
plan: "02"
subsystem: observability
tags: [prometheus, http, metrics, health-check, grafana]
dependency_graph:
  requires: ["08-01"]
  provides: ["OBS-02", "OBS-03"]
  affects: ["internal/observability", "internal/bridge", "internal/sip", "cmd/sipgate-sip-stream-bridge"]
tech_stack:
  added:
    - "github.com/prometheus/client_golang v1.23.2 — Prometheus metrics + promhttp handler"
  patterns:
    - "Custom prometheus.Registry (not DefaultRegisterer) — excludes Go runtime noise from /metrics"
    - "nil-safe metrics guard pattern (if m.metrics != nil) — safe for tests with nil metrics"
    - "Gauge with _total suffix for active_calls_total — matches OBS-03 literal requirement"
key_files:
  created:
    - "internal/observability/metrics.go — Metrics struct + NewMetrics() with 5 metrics on custom registry"
  modified:
    - "internal/config/config.go — added HTTPPort (HTTP_PORT, default 8080)"
    - "internal/bridge/manager.go — metrics field + NewCallManager signature + active_calls Inc/Dec"
    - "internal/bridge/session.go — metrics field + RTPRx.Inc() + RTPTx.Inc() + WSReconnects.Inc()"
    - "internal/sip/registrar.go — metrics field + NewRegistrar signature + SIPRegStatus.Set(0/1)"
    - "internal/sip/registrar_test.go — updated NewRegistrar call to pass nil metrics"
    - "cmd/sipgate-sip-stream-bridge/main.go — metrics creation + HTTP server + httpServer.Shutdown in drain sequence"
decisions:
  - "[08-02] Custom prometheus.Registry (not DefaultRegisterer) — excludes Go runtime/process metrics from scrape; only 5 sipgate-sip-stream-bridge metrics exposed"
  - "[08-02] active_calls_total as Gauge with _total suffix — OBS-03 specifies name literally; Gauge semantics correct (goes up/down); Prometheus client allows it"
  - "[08-02] nil-safe metrics guard in all hot paths — enables zero-value construction in tests without injecting a real registry"
  - "[08-02] RTPTx increments on silence frames too — rtpPacer sends silence for NAT traversal; all WriteTo successes counted as sent packets"
metrics:
  duration: "~4min"
  completed_date: "2026-03-04"
  tasks_completed: 2
  tasks_total: 2
  files_created: 1
  files_modified: 6
---

# Phase 08 Plan 02: HTTP Observability Layer Summary

**One-liner:** Prometheus custom registry with 5 sipgate-sip-stream-bridge metrics + /health JSON + /metrics endpoint wired into bridge/registrar hot paths and main.go graceful shutdown.

## Tasks Completed

| # | Task | Commit | Key Files |
|---|------|--------|-----------|
| 1 | Create observability package, wire metric increments | 4bb6455 | internal/observability/metrics.go, manager.go, session.go, registrar.go, config.go |
| 2 | Wire HTTP server with /health and /metrics into main.go | 56b2459 | cmd/sipgate-sip-stream-bridge/main.go |

## What Was Built

**internal/observability/metrics.go (new):**
- `Metrics` struct with 5 fields: `ActiveCalls` (Gauge), `SIPRegStatus` (Gauge), `RTPRx` (Counter), `RTPTx` (Counter), `WSReconnects` (Counter)
- `NewMetrics()` creates a `prometheus.NewRegistry()` (custom, not default) and registers all 5 metrics
- Metric names match OBS-03 exactly: `active_calls_total`, `sip_registration_status`, `rtp_packets_received_total`, `rtp_packets_sent_total`, `ws_reconnect_attempts_total`

**Metric increment wiring:**
- `active_calls_total`: Inc after `m.sessions.Store(callID, session)` in StartSession; Dec inside defer alongside `m.sessions.Delete(callID)`
- `rtp_packets_received_total`: Inc in rtpReader after PCMU path (PT==0) before enqueue
- `rtp_packets_sent_total`: Inc in rtpPacer after successful `rtpConn.WriteTo`
- `ws_reconnect_attempts_total`: Inc at top of reconnect() for loop before backoff select
- `sip_registration_status`: Set(1) alongside `r.registered.Store(true)` in doRegister; Set(0) alongside `r.registered.Store(false)` in reregisterLoop error path and Unregister

**cmd/sipgate-sip-stream-bridge/main.go HTTP server:**
- `/health` returns `{"registered":bool,"activeCalls":int}` (always HTTP 200, Content-Type: application/json)
- `/metrics` uses `promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})` — custom registry only
- Server starts after SIP registration succeeds; logs `http_port` field
- `httpServer.Shutdown(2s)` added as step 4 in graceful shutdown sequence (after Unregister, before "shutdown complete")

## Shutdown Sequence (final)

1. `handler.SetShutdown()` — reject new INVITEs
2. `callManager.DrainAll(8s)` — BYE all active sessions
3. `registrar.Unregister(5s)` — de-register from sipgate
4. `httpServer.Shutdown(2s)` — drain in-flight HTTP scrapes (NEW)
5. `logger.Info().Msg("shutdown complete")`
6. `defer agent.UA.Close()` — runs last

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] registrar_test.go NewRegistrar call missing metrics argument**
- **Found during:** Task 1 build verification (`go vet` failed)
- **Issue:** `registrar_test.go:27` called `NewRegistrar(nil, cfg, log)` — missing the new `metrics *observability.Metrics` argument after signature update
- **Fix:** Updated call to `NewRegistrar(nil, cfg, log, nil)` — nil metrics is valid; all hot paths guard with `if r.metrics != nil`
- **Files modified:** `internal/sip/registrar_test.go`
- **Commit:** 4bb6455

## Self-Check: PASSED

Files exist:
- internal/observability/metrics.go: FOUND
- internal/bridge/manager.go: FOUND (metrics field + Inc/Dec)
- internal/bridge/session.go: FOUND (metrics field + RTPRx/RTPTx/WSReconnects)
- internal/sip/registrar.go: FOUND (metrics field + SIPRegStatus)
- cmd/sipgate-sip-stream-bridge/main.go: FOUND (httpServer + Shutdown)

Commits exist:
- 4bb6455: FOUND (Task 1)
- 56b2459: FOUND (Task 2)

Build: go build ./... exits 0
Vet: go vet ./... exits 0
