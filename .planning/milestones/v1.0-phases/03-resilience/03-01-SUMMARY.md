---
phase: 03-resilience
plan: 01
subsystem: bridge
tags: [websocket, reconnect, backoff, rtp, silence, callManager]

# Dependency graph
requires:
  - phase: 02-core-bridge
    provides: CallSession, createWsClient, RtpHandler — full audio bridge infrastructure
provides:
  - WS reconnect loop with 30s budget and 1s/2s/4s exponential backoff on mid-call drops
  - wsReconnecting gate on CallSession that drops inbound RTP during reconnect window
  - RTP silence injection (μ-law 0xFF, 20ms interval) to prevent caller dead-air during reconnect
  - startWsReconnectLoop private method on CallManager with BYE race protection
affects: [03-02-resilience, any future phase touching CallSession or WS bridge]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Recursive setTimeout backoff loop with budget-check on each attempt"
    - "wsReconnecting boolean flag on session object as an RTP audio gate"
    - "cleanup() closure pattern for clearing silence interval before re-wiring"
    - "BYE-race guard: sessions.has(callId) checked at top of each reconnect attempt"

key-files:
  created: []
  modified:
    - src/bridge/callManager.ts

key-decisions:
  - "Route rtp.on('audio') and rtp.on('dtmf') via session.ws (not closure-captured ws) so handlers pick up new WsClient after reconnect without re-registration"
  - "Drop inbound RTP during reconnect (wsReconnecting gate) — consistent with Phase 2 drop-not-buffer policy"
  - "Re-call createWsClient with same WsCallParams — WSR-02 satisfied automatically because createWsClient always sends connected+start on open"
  - "Capture wsParams from fromUri/toUri/sipCallId local vars before ws.onDisconnect registration so closure retains correct values"
  - "cleanup() called before session.wsReconnecting=false to prevent silence/audio interleave (Pitfall 3 avoidance)"

patterns-established:
  - "Pattern: startWsReconnectLoop — private async method on CallManager; place after terminateSession; recursive attempt(n) inner function with budget check on catch"
  - "Pattern: wsReconnecting gate — first line of rtp.on('audio') handler; simple boolean check; no buffering"

requirements-completed: [WSR-01, WSR-02, WSR-03]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 3 Plan 1: WS Reconnect Loop Summary

**Exponential backoff WS reconnect loop (30s, 1s/2s/4s cap) with RTP silence injection and wsReconnecting drop gate — replaces immediate BYE on WS disconnect**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-03-03T13:29:06Z
- **Completed:** 2026-03-03T13:31:37Z
- **Tasks:** 1 of 1
- **Files modified:** 1

## Accomplishments

- Replaced `ws.onDisconnect → terminateSession` with a reconnect loop that keeps the SIP call alive during transient WS drops (WSR-01)
- Re-calling `createWsClient` with same `streamSid`/`callSid` parameters satisfies WSR-02 — backend receives fresh `connected` + `start` events automatically on reconnect
- `wsReconnecting` boolean flag on `CallSession` gates `rtp.on('audio')` handler so inbound RTP is silently dropped during the reconnect window (WSR-03)
- Silence interval sends μ-law 0xFF packets every 20ms throughout reconnect to prevent caller dead-air
- BYE race protection: each reconnect attempt checks `sessions.has(session.callId)` and exits cleanly if session was terminated externally

## Task Commits

Each task was committed atomically:

1. **Task 1: Add wsReconnecting to CallSession + implement startWsReconnectLoop** - `1809da0` (feat)

**Plan metadata:** (to be committed with docs)

## Files Created/Modified

- `src/bridge/callManager.ts` - Added `wsReconnecting: boolean` to `CallSession` interface and session literal; added wsReconnecting gate to `rtp.on('audio')`; routed audio and DTMF via `session.ws`; captured `wsParams` and replaced `ws.onDisconnect` with reconnect loop trigger; added `startWsReconnectLoop` private method; imported `WsCallParams` type

## Decisions Made

- Route `rtp.on('audio')` and `rtp.on('dtmf')` via `session.ws` (not closure-captured `ws`) — avoids DTMF stale reference pitfall after reconnect without needing to re-register listeners
- Import `WsCallParams` as a named type import (was not previously imported explicitly in callManager.ts)
- `cleanup()` called before `session.wsReconnecting = false` on success — prevents silence interval and real audio from interleaving during the assignment window

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None — TypeScript compiled cleanly on the first attempt.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- WSR-01, WSR-02, WSR-03 all satisfied
- `startWsReconnectLoop` is fully wired; `session.ws` is hot-swappable post-reconnect
- Ready for Phase 3 Plan 2 (03-02) — FD leak test or further resilience work

---
*Phase: 03-resilience*
*Completed: 2026-03-03*
