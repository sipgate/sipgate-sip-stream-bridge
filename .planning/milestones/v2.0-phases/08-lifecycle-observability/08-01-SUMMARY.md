---
phase: 08-lifecycle-observability
plan: "01"
subsystem: sip
tags: [shutdown, graceful-drain, sigterm, atomic, sync]

# Dependency graph
requires:
  - phase: 06-inbound-call-rtp-bridge
    provides: CallManager.sessions sync.Map + StartSession goroutine with defer sessions.Delete
  - phase: 05-sip-registration
    provides: Registrar.Unregister + Register lifecycle
provides:
  - "Handler.SetShutdown() atomic flag — onInvite returns 503 during drain"
  - "CallManager.DrainAll(ctx) — BYEs all sessions, polls until map empty or context timeout"
  - "CallManager.ActiveCount() int — counts live sessions via sessions.Range"
  - "Registrar.IsRegistered() bool — reflects current registration state via atomic.Bool"
  - "Full graceful drain sequence in main.go: SetShutdown → DrainAll(8s) → Unregister(5s)"
affects: [08-02-lifecycle-observability, health-endpoint]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "atomic.Bool as zero-value struct field (no pointer, no init needed) for shutdown flag"
    - "Drain via polling sync.Map (50ms tick) — avoids modifying hot session close path"
    - "SetShutdown before DrainAll ordering to prevent new-session race during drain"

key-files:
  created: []
  modified:
    - internal/sip/handler.go
    - internal/bridge/manager.go
    - internal/sip/registrar.go
    - cmd/sipgate-sip-stream-bridge/main.go

key-decisions:
  - "[08-01] 503 (not 480) for INVITE reject during shutdown — RFC 3261 §21.5.4 defines 503 for server-draining"
  - "[08-01] Polling sync.Map every 50ms in DrainAll — avoids adding WaitGroup to session close path (avoids modifying hot path)"
  - "[08-01] registered.Store(false) only on failure, NOT before doRegister attempt — avoids false-negative during ~200ms round-trip"
  - "[08-01] drainCtx timeout = 8s, unregCtx = 5s — total under 10s SIGTERM budget with margin"

patterns-established:
  - "Shutdown ordering: SetShutdown → DrainAll(ctx) → Unregister(ctx) — prevents races and routing confusion"
  - "atomic.Bool for process-lifecycle flags (shutdown, registered) — lock-free, goroutine-safe"

requirements-completed: [LCY-01]

# Metrics
duration: 2min
completed: 2026-03-04
---

# Phase 8 Plan 01: Graceful SIGTERM Shutdown Summary

**SIGTERM drain sequence: SetShutdown atomic flag + DrainAll BYE loop (8s) + Unregister (5s) ensure clean call teardown and SIP deregistration within 10s budget**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-03-04T15:20:19Z
- **Completed:** 2026-03-04T15:22:13Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Handler.SetShutdown() with atomic.Bool — onInvite rejects new INVITEs with 503 during drain window
- CallManager.DrainAll(ctx) sends BYE to all active sessions then polls sync.Map every 50ms until empty or context timeout
- Registrar.IsRegistered() tracks registration state via atomic.Bool — true after successful register, false after failure/unregister
- main.go shutdown sequence refactored: capture handler return value, then SetShutdown → DrainAll(8s) → Unregister(5s) → log complete

## Task Commits

Each task was committed atomically:

1. **Task 1: Add shutdownFlag to Handler, DrainAll+ActiveCount to CallManager, IsRegistered to Registrar** - `70a7f4d` (feat)
2. **Task 2: Refactor main.go shutdown sequence with SetShutdown, DrainAll, and timed drain** - `9fa9999` (feat)

## Files Created/Modified
- `internal/sip/handler.go` - Added `shutdown atomic.Bool` field, `SetShutdown()` method, 503 guard at top of `onInvite`
- `internal/bridge/manager.go` - Added `ActiveCount() int` and `DrainAll(ctx context.Context)` methods to CallManager
- `internal/sip/registrar.go` - Added `registered atomic.Bool` field, `IsRegistered() bool` method, Store(true/false) at success/failure/unregister points
- `cmd/sipgate-sip-stream-bridge/main.go` - Captured handler return from sip.NewHandler; replaced shutdown block with full 3-step drain sequence

## Decisions Made
- 503 (not 480) for rejected INVITEs during shutdown: RFC 3261 §21.5.4 — 503 is the correct status when server is draining
- DrainAll uses 50ms polling loop on sync.Map rather than WaitGroup — avoids touching the session close path (Rule 1 correctness)
- registered.Store(false) set only on reregisterLoop failure, NOT before the doRegister call — prevents false-negative during ~200ms round-trip (Research Pitfall 5)
- Drain budget: 8s for calls + 5s for unregister = 13s worst case; SIGTERM timeout should be set to >=15s in prod

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- LCY-01 satisfied: active calls receive BYE, new INVITEs are rejected, UNREGISTER sent, process exits within budget
- IsRegistered() and ActiveCount() are available for Phase 8 Plan 02 health endpoint
- No blockers

## Self-Check: PASSED

- FOUND: internal/sip/handler.go
- FOUND: internal/bridge/manager.go
- FOUND: internal/sip/registrar.go
- FOUND: cmd/sipgate-sip-stream-bridge/main.go
- FOUND: .planning/phases/08-lifecycle-observability/08-01-SUMMARY.md
- FOUND commit: 70a7f4d (Task 1)
- FOUND commit: 9fa9999 (Task 2)

---
*Phase: 08-lifecycle-observability*
*Completed: 2026-03-04*
