---
phase: 10-go-sip-options-keepalive
plan: 02
subsystem: sip
tags: [sipgo, sync.Mutex, options-keepalive, prometheus, go]

# Dependency graph
requires:
  - phase: 10-01
    provides: SIPOptionsInterval time.Duration field in Config; SIPOptionsFailures prometheus.Counter in Metrics
  - phase: 05-go-sip-registration
    provides: Registrar struct, doRegister, reregisterLoop, Register() in registrar.go
provides:
  - optionsKeepaliveLoop goroutine with 2-failure threshold state machine in registrar.go
  - sendOptions helper: out-of-dialog SIP OPTIONS with 10s timeout
  - isOptionsFailure, isOptionsAuth, applyOptionsResponse: pure classification functions
  - sync.Mutex serializing all concurrent doRegister calls
  - reregisterLoop mutex-guarded doRegister
  - Full unit tests: TestOptionsKeepalive_ClassifyFailure, _ClassifyAuth, _ThresholdLogic, _ContextCancel
affects:
  - 11-node-sip-options-keepalive
  - any phase reading Registrar struct layout

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Pure state machine functions (applyOptionsResponse) extracted for testability — no goroutine needed in tests"
    - "sync.Mutex on Registrar guards all doRegister call sites; OPTIONS request itself is out-of-dialog and not guarded"
    - "SIPOptionsFailures.Inc() called on every failure, including the threshold-triggering one"
    - "Context-cancel test uses 500ms ticker + 50ms cancel — avoids nil-client panic before first tick"

key-files:
  created: []
  modified:
    - go/internal/sip/registrar.go
    - go/internal/sip/registrar_test.go

key-decisions:
  - "applyOptionsResponse is a pure function (no side effects) — enables table-driven unit tests without a live SIP server"
  - "auth responses (401/407) reset consecutive-failure counter to 0 — consistent with isOptionsAuth behavior"
  - "SIPOptionsFailures.Inc() placed before the triggerRegister branch so the threshold-triggering failure is also counted"
  - "optionsKeepaliveLoop uses 500ms ticker + 50ms cancel in context test to avoid nil client.Do panic"

patterns-established:
  - "Unexported pure helper functions for state machine logic enable table-driven tests in same package"
  - "Mutex lock/unlock inline at each doRegister call site (not deferred) — consistent across reregisterLoop and optionsKeepaliveLoop"

requirements-completed: [OPTS-01, OPTS-02, OPTS-03, OPTS-04]

# Metrics
duration: 3min
completed: 2026-03-05
---

# Phase 10 Plan 02: OPTIONS Keepalive Implementation Summary

**optionsKeepaliveLoop with 2-failure threshold state machine, sync.Mutex on all doRegister call sites, and pure helper functions enabling table-driven tests without a live SIP server**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-05T13:20:03Z
- **Completed:** 2026-03-05T13:23:05Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- Complete `optionsKeepaliveLoop` goroutine: ticks at `optionsInterval`, runs `sendOptions`, drives `applyOptionsResponse` state machine, calls `doRegister` on threshold
- `sync.Mutex` added to `Registrar` struct; all `doRegister` call sites in both `reregisterLoop` and `optionsKeepaliveLoop` are mutex-guarded — `go test -race` passes
- Pure helper functions (`isOptionsFailure`, `isOptionsAuth`, `applyOptionsResponse`) extracted for testability — entire state machine covered by table-driven tests with zero goroutines
- `SIPOptionsFailures.Inc()` fires on every failure including the threshold-triggering one

## Task Commits

Each task was committed atomically:

1. **Task 1: Add Wave 0 test stubs for OPTIONS keepalive state machine** - `c3a3395` (test)
2. **Task 2: Implement optionsKeepaliveLoop and mutex-guard doRegister** - `a8cedfa` (feat)

## Files Created/Modified
- `go/internal/sip/registrar.go` - Added `mu sync.Mutex`, `optionsInterval time.Duration` to Registrar; `isOptionsFailure`, `isOptionsAuth`, `applyOptionsResponse`, `sendOptions`, `optionsKeepaliveLoop`; mutex-guarded `doRegister` in `reregisterLoop`; launched `optionsKeepaliveLoop` from `Register()`
- `go/internal/sip/registrar_test.go` - Replaced stubs with full table-driven tests: `TestOptionsKeepalive_ClassifyFailure`, `TestOptionsKeepalive_ClassifyAuth`, `TestOptionsKeepalive_ThresholdLogic`, `TestOptionsKeepalive_ContextCancel`

## Decisions Made
- `applyOptionsResponse` is a pure function — allows table-driven tests without a live SIP server or goroutine harness
- auth responses (401/407) reset consecutive-failure counter to 0 per CONTEXT.md locked decision
- `SIPOptionsFailures.Inc()` placed outside the `triggerRegister` branch so the threshold-triggering failure is also counted (CONTEXT.md: increment on every failure)
- `TestOptionsKeepalive_ContextCancel` uses 500ms ticker + 50ms cancel to guarantee ctx is cancelled before first tick, avoiding nil `client.Do` panic

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None — implementation matched plan spec precisely. Build and race-detector tests passed on first attempt.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 10 implementation is complete: config field (10-01), metrics counter (10-01), and keepalive goroutine (10-02) are all in place
- `go test -race ./internal/sip/... ./internal/config/... ./internal/observability/...` exits 0
- Phase 11 (Node.js SIP OPTIONS keepalive) can proceed independently

---
*Phase: 10-go-sip-options-keepalive*
*Completed: 2026-03-05*

## Self-Check: PASSED

- go/internal/sip/registrar.go: FOUND
- go/internal/sip/registrar_test.go: FOUND
- .planning/phases/10-go-sip-options-keepalive/10-02-SUMMARY.md: FOUND
- Commit c3a3395 (test stubs): FOUND
- Commit a8cedfa (implementation): FOUND
