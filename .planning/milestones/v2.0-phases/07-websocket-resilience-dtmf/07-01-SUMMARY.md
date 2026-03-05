---
phase: 07-websocket-resilience-dtmf
plan: 01
subsystem: bridge
tags: [websocket, reconnect, exponential-backoff, goroutines, sync.Once, net.Conn]

requires:
  - phase: 06-inbound-call-rtp-bridge
    provides: "CallSession with 4-goroutine model (rtpReader, wsPacer, wsToRTP, rtpPacer), ws.go helpers"

provides:
  - "wsSignal type in ws.go: sync.Once-guarded single-fire channel for WS goroutine failure"
  - "handshake() method in session.go: sends connected+start on any new wsConn"
  - "reconnect() method in session.go: 1s/2s/4s exponential backoff, 30s budget, context-aware"
  - "Refactored run() with WS reconnect loop: WS errors no longer trigger immediate BYE"
  - "wsPacer/wsToRTP updated to accept per-connection wsWg and wsSignal parameters"

affects:
  - "07-02-PLAN.md (DTMF phase — will add dtmfQueue channel and build on same session.go)"
  - "phase 08 (lifecycle/shutdown behavior changed: s.wg tracks only 2 goroutines now)"

tech-stack:
  added: []
  patterns:
    - "Reconnect loop in run(): RTP goroutines are persistent; WS goroutines are per-connection"
    - "wsSignal pattern: sync.Once-guarded chan struct{} for double-close-safe multi-goroutine signaling"
    - "Per-connection wsWg separate from session s.wg to allow independent drain on reconnect"
    - "Budget context: context.WithTimeout(sessionCtx, 30s) used inside reconnect() for bounded retry"

key-files:
  created: []
  modified:
    - internal/bridge/ws.go
    - internal/bridge/session.go
    - internal/bridge/ws_test.go

key-decisions:
  - "[07-01] wsSignal uses sync.Once to guard channel close — prevents double-close panic when both wsPacer and wsToRTP fail simultaneously"
  - "[07-01] s.wg now tracks ONLY rtpReader + rtpPacer (2 goroutines); wsPacer + wsToRTP use per-connection local wsWg"
  - "[07-01] wsToRTP WS read error signals reconnect via sig.Signal() instead of calling dlg.Bye(); only 'stop' event triggers BYE"
  - "[07-01] wsPacer write error calls sig.Signal() and returns; run() loop handles reconnect decision"
  - "[07-01] reconnect+handshake wrapped in inner loop: both must succeed before WS goroutines restart"
  - "[07-01] Shutdown sequence: SetReadDeadline → wsWg.Wait() → rtpConn.Close() → s.wg.Wait() → sendStop → wsConn.Close()"
  - "[07-01] rtpConn is NEVER closed during reconnect path — only on final session shutdown (Pitfall 3)"
  - "[07-01] defer wsConn.Close() removed from run(); explicit close in each return path (wsConn reassigned in loop)"

patterns-established:
  - "Per-connection WaitGroup pattern: create wsWg inside the reconnect for-loop, pass to WS goroutines"
  - "Signal-then-drain: set read deadline to unblock wsToRTP, close conn to fail wsPacer, then wsWg.Wait()"

requirements-completed: [WSR-01, WSR-02, WSR-03]

duration: 4min
completed: 2026-03-04
---

# Phase 7 Plan 01: WebSocket Resilience - Reconnect Loop Summary

**WS reconnect loop with 1s/2s/4s backoff and wsSignal guard — calls survive transient WebSocket disconnects without SIP BYE**

## Performance

- **Duration:** ~4 min
- **Started:** 2026-03-04T14:50:31Z
- **Completed:** 2026-03-04T14:54:07Z
- **Tasks:** 2
- **Files modified:** 3

## Accomplishments

- Added `wsSignal` type (sync.Once-guarded channel) to ws.go — prevents double-close panic when both WS goroutines fail simultaneously
- Added `handshake()` method to session.go — re-sends connected+start on every new wsConn (WSR-02)
- Added `reconnect()` method — 1s/2s/4s exponential backoff, 30s budget, context-aware via budget ctx
- Refactored `run()` with reconnect loop: WS errors trigger reconnect instead of immediate SIP BYE (WSR-01)
- Updated `wsPacer` and `wsToRTP` to use per-connection `wsWg` and `wsSignal` — clean goroutine lifecycle per connection
- `rtpReader` and `rtpPacer` remain persistent throughout reconnects (WSR-03 — RTP drops during reconnect handled by bounded queue)
- All 12 tests pass with -race; no data races

## Task Commits

Each task was committed atomically:

1. **Task 1: Add wsSignal type + handshake() helper** - `3f3db62` (feat)
2. **Task 2: Add reconnect() method + refactor run()** - `24cc80e` (feat)

**Plan metadata:** (docs commit follows this summary)

_Note: Both tasks used TDD — tests written first, then implementation._

## Files Created/Modified

- `internal/bridge/ws.go` - Added wsSignal type (sync.Once, Signal(), Done()), added sync import
- `internal/bridge/session.go` - Refactored run() with reconnect loop; added handshake(), reconnect(); updated wsPacer/wsToRTP signatures; s.wg now tracks only 2 RTP goroutines
- `internal/bridge/ws_test.go` - Added TestWsSignal_MultipleSignalsNoPanic, TestWsSignal_DoneClosedAfterSignal, TestHandshake_SendsConnectedThenStart

## Decisions Made

- `wsSignal` uses `sync.Once` to guard `close(ch)` — prevents double-close panic (Pitfall 1 from research)
- `s.wg` changed from tracking 4 goroutines to 2 (only rtpReader + rtpPacer); wsPacer/wsToRTP use local `wsWg` per connection iteration
- `wsToRTP` WS read error now calls `sig.Signal()` (not `dlg.Bye()`) — WS failure is now a reconnect trigger, not a SIP teardown trigger; only explicit "stop" event causes BYE
- Reconnect+handshake wrapped in an inner loop so a failed handshake immediately retries reconnect within the same budget window
- `defer wsConn.Close()` removed from run() because wsConn is reassigned in the loop; each return path closes explicitly
- Shutdown sequence: `SetReadDeadline` (unblocks wsToRTP) → `wsWg.Wait()` → `rtpConn.Close()` (unblocks rtpReader) → `s.wg.Wait()` → `sendStop` → `wsConn.Close()` — preserves sendStop behavior from Phase 6

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None — implementation followed research patterns directly.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Phase 07-02 (DTMF): session.go is ready for `dtmfQueue chan string` addition and DTMF parsing in rtpReader
- The `wsPacer` select loop is the correct insertion point for DTMF forwarding (add `case digit := <-s.dtmfQueue:` alongside the ticker case)
- No new dependencies needed for Phase 07-02

## Self-Check: PASSED

- internal/bridge/ws.go: FOUND
- internal/bridge/session.go: FOUND
- internal/bridge/ws_test.go: FOUND
- .planning/phases/07-websocket-resilience-dtmf/07-01-SUMMARY.md: FOUND
- Commit 3f3db62 (Task 1): FOUND
- Commit 24cc80e (Task 2): FOUND
- go build ./...: PASS
- go test ./internal/bridge/... -race: PASS (12 tests)

---
*Phase: 07-websocket-resilience-dtmf*
*Completed: 2026-03-04*
