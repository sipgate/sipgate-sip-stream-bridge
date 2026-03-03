---
phase: 02-core-bridge
plan: 05
subsystem: infra
tags: [sip, node, typescript, graceful-shutdown, callmanager, sigterm]

# Dependency graph
requires:
  - phase: 02-core-bridge
    provides: "CallManager (terminateAll, getCallbacks, setSipHandle), SipHandle (unregister, sendRaw, stop)"
provides:
  - "Fully wired entrypoint: CallManager + SipHandle + SIGTERM/SIGINT graceful shutdown"
  - "Complete Phase 2 bidirectional audio bridge connected end-to-end"
affects: [03-resilience, 04-observability]

# Tech tracking
tech-stack:
  added: []
  patterns: [graceful-shutdown, signal-handler, drain-timeout]

key-files:
  created: []
  modified: ["src/index.ts"]

key-decisions:
  - "SIGTERM and SIGINT share same shutdown() function — avoids handler drift"
  - "5-second drain timeout uses setTimeout cleared in finally — guarantees process.exit even if unregister hangs"
  - "shutdown sequence: terminateAll() first (BYE + WS close), then unregister() — per CONTEXT.md locked decision"
  - "void shutdown('SIGTERM') pattern suppresses unhandled-rejection lint warnings on async signal handlers"

patterns-established:
  - "Signal handlers: always use void + async function, clear timeout in finally"
  - "Wiring order: instantiate CallManager → pass getCallbacks() to createSipUserAgent → call setSipHandle() after resolve"

requirements-completed: [LCY-01, CON-02]

# Metrics
duration: 1min
completed: 2026-03-03
---

# Phase 2 Plan 05: Wired Entrypoint and Graceful Shutdown Summary

**src/index.ts fully wired: CallManager instantiated, SipHandle callbacks connected, SIGTERM/SIGINT graceful shutdown with 5-second drain timeout**

## Performance

- **Duration:** 1 min
- **Started:** 2026-03-03T13:17:32Z
- **Completed:** 2026-03-03T13:18:11Z
- **Tasks:** 1
- **Files modified:** 1

## Accomplishments
- CallManager instantiated in main() with child logger and config
- getCallbacks() passed to createSipUserAgent as third argument for INVITE/ACK/BYE routing
- setSipHandle(sipHandle) called after createSipUserAgent resolves (SIP registered and ready)
- Graceful shutdown handler registered for both SIGTERM and SIGINT (LCY-01)
- Shutdown sequence: terminateAll() (BYE all calls + close all WS) then unregister() then process.exit(0)
- 5-second drain timeout forces exit if cleanup stalls; cleared in finally to prevent leaks
- pnpm typecheck and pnpm build both pass clean

## Task Commits

Each task was committed atomically:

1. **Task 1: Wire CallManager into entrypoint and implement graceful shutdown** - `0d5dba9` (feat)

**Plan metadata:** (docs commit follows)

## Files Created/Modified
- `src/index.ts` - Fully wired entrypoint with CallManager, SipHandle, SIGTERM/SIGINT shutdown

## Decisions Made
- SIGTERM and SIGINT use the same `shutdown()` function to avoid handler drift between development (Ctrl+C) and production (container stop)
- `void shutdown('SIGTERM')` pattern used to suppress unhandled-promise-rejection lint warnings on async signal handlers
- 5-second drain timeout set with `setTimeout` inside `shutdown()`, cleared in `finally` block before `process.exit(0)` — guarantees exit even if `unregister()` hangs
- Shutdown sequence follows CONTEXT.md locked decision: `terminateAll()` first (BYE + WS.close in parallel), then `unregister()`, then `process.exit(0)`

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Phase 2 complete: full bidirectional audio bridge (RTP RtpHandler + WsClient + CallManager + SipUserAgent + wired entrypoint)
- All Phase 2 requirements traceable: SIP-03/04/05 (INVITE/BYE routing via getCallbacks), WSB-01-07 (WsClient), CON-01/02 (CallSession Map + graceful shutdown), LCY-01 (SIGTERM/SIGINT handler)
- Ready for Phase 3 (resilience: reconnect, retry, health checks)
- No blockers

## Self-Check: PASSED

- FOUND: src/index.ts
- FOUND: .planning/phases/02-core-bridge/02-05-SUMMARY.md
- FOUND: commit 0d5dba9 (feat(02-05): wire CallManager + graceful shutdown into entrypoint)

---
*Phase: 02-core-bridge*
*Completed: 2026-03-03*
