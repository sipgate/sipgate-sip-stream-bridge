---
phase: 11-node.js-equivalents
plan: 02
subsystem: api
tags: [websocket, twilio-media-streams, mark-clear, vitest, tdd, typescript]

# Dependency graph
requires:
  - phase: 11-01
    provides: Vitest test infrastructure, wsClient.mark.test.ts stubs with it.todo
  - phase: 09-go-bridge-mark-clear
    provides: Go behavioral reference for mark/clear queue semantics (MARK-01/02/03)
provides:
  - Extended WsClient interface with onMark, sendMark, sendClear
  - Tagged-union DrainItem type for mixed audio/mark queue
  - makeDrainForTest export for unit testing drain internals
  - Mark sentinel ordering: echoes after preceding audio frames drain
  - MARK-02 fast-path: immediate echo when queue idle
  - MARK-03 clear: stop() echoes pending marks before flush
  - callManager wires ws.onMark on initial connect AND reconnect
affects: [12-integration, production-deployment]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - Tagged-union queue: Buffer | { markName: string } mixed queue for paced drain with mark sentinels
    - Test export pattern: makeDrainForTest re-exports internal function for unit test access without breaking production types
    - Mark echo routing: closure-captured markHandler set by onMark(), called from onMarkReady callback in makeDrain
    - onMark re-wire on reconnect: startWsReconnectLoop wires newWs.onMark after session.ws = newWs so sendMark routes to new connection

key-files:
  created: []
  modified:
    - node/src/ws/wsClient.ts
    - node/src/bridge/callManager.ts
    - node/test/wsClient.mark.test.ts

key-decisions:
  - "makeDrainForTest export: exposes internal makeDrain for unit tests — avoids moving makeDrain to a separate file while keeping production API clean"
  - "Mark sentinel no 20ms wait: after echoing a mark sentinel the next item is processed at delay=0, not full 20ms packet interval — marks are protocol signals not audio frames"
  - "sendClear delegates to outboundDrain.stop(): reuses the existing flush+echo-marks behavior in stop() rather than duplicating logic"

patterns-established:
  - "Tagged-union drain queue: use Buffer.isBuffer() discriminant to distinguish audio frames from mark sentinels in the same queue"
  - "Fast-path sentinel echo: when queue.length === 0 && timer === null, call onMarkReady synchronously without enqueuing"

requirements-completed: [MRKN-01, MRKN-02, MRKN-03]

# Metrics
duration: 8min
completed: 2026-03-05
---

# Phase 11 Plan 02: Node.js mark/clear Protocol Summary

**Tagged-union outbound drain with mark sentinel ordering and immediate clear-echo, Node.js parity with Go Phase 9 MARK-01/02/03 behavior**

## Performance

- **Duration:** 8 min
- **Started:** 2026-03-05T16:58:00Z
- **Completed:** 2026-03-05T17:06:00Z
- **Tasks:** 2 (RED + GREEN TDD cycle)
- **Files modified:** 3

## Accomplishments
- Extended `WsClient` interface with `onMark`, `sendMark`, `sendClear` methods
- Replaced `Buffer[]` queue with `DrainItem[]` tagged-union queue in `makeDrain`
- All three MRKN requirements pass: mark ordering, fast-path, clear-echo
- `callManager.ts` wires `ws.onMark` in both `handleInvite` and `startWsReconnectLoop`
- TypeScript compiles cleanly (`tsc --noEmit` exits 0)

## Task Commits

Each task was committed atomically:

1. **Task 1: RED — failing mark/clear tests** - `95e1c43` (test)
2. **Task 2: GREEN — wsClient + callManager implementation** - `0741a57` (feat)

_TDD plan: RED commit first (failing assertions), GREEN commit second (passing implementation)_

## Files Created/Modified
- `node/src/ws/wsClient.ts` - DrainItem tagged union, extended makeDrain, onMark/sendMark/sendClear, mark/clear message dispatch, makeDrainForTest export
- `node/src/bridge/callManager.ts` - ws.onMark wiring in handleInvite and startWsReconnectLoop
- `node/test/wsClient.mark.test.ts` - Real expect() assertions replacing it.todo stubs

## Decisions Made
- **makeDrainForTest export:** Exposes the internal `makeDrain` function for unit tests by name, allowing tests to construct drain instances without a real WebSocket. Avoids moving `makeDrain` to a separate module.
- **Mark sentinel no 20ms wait:** When a mark sentinel is processed from the queue, the next item is scheduled at `delay=0` (not 20ms). Marks are protocol signals, not audio frames requiring pacing.
- **sendClear delegates to outboundDrain.stop():** The existing `stop()` already echoes pending marks before flushing — `sendClear()` simply calls it. The drain restarts automatically on the next `enqueue()` call.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Node.js mark/clear protocol complete — MRKN-01/02/03 all green
- Phase 11 plan 03 (if any) can build on the extended WsClient interface
- End-to-end mark/clear flow ready: Go sends mark/clear events, Node.js wsClient echoes them back, callManager wires the handler on connect and reconnect

---
*Phase: 11-node.js-equivalents*
*Completed: 2026-03-05*
