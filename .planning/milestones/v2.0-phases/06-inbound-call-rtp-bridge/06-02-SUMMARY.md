---
phase: 06-inbound-call-rtp-bridge
plan: 02
subsystem: bridge
tags: [rtp, portpool, callmanager, callsession, goroutine, tdd, websocket, pion-rtp, gobwas-ws, sync-map]

# Dependency graph
requires:
  - phase: 06-inbound-call-rtp-bridge
    plan: 01
    provides: "CallerSDP type (ParseCallerSDP), CallManagerIface defined in sip package, handler.go with StartSession goroutine launch"
  - phase: 04-go-scaffold
    provides: "Config struct with WSTargetURL, SDPContactIP, RTPPortMin, RTPPortMax"

provides:
  - "internal/bridge/manager.go — PortPool (channel-based bounded pool, O(1) acquire/release) + CallManager (sync.Map session registry, satisfies sip.CallManagerIface)"
  - "internal/bridge/session.go — CallSession per-call state; run() lifecycle with RTP socket, WS connect, bidirectional audio goroutines; wg.Wait() before sendStop (write-safety guarantee)"
  - "internal/bridge/ws.go — stub signatures for dialWS/sendConnected/sendStart/sendStop/writeJSON (replaced in 06-03)"

affects: [06-03-websocket-bridge, 08-lifecycle]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "PortPool: buffered chan int for O(1), lock-free acquire/release — non-blocking select default on exhaustion"
    - "CallManager satisfies sip.CallManagerIface via method signatures on *CallManager — no explicit interface declaration in bridge package"
    - "CallSession.run(): sessionCtx derived from dlg.Context() so BYE cancels session automatically"
    - "WRITE-SAFETY: wg.Wait() drains rtpToWS before sendStop writes to wsConn — gobwas/ws concurrent write race prevention (RESEARCH Pitfall 5)"
    - "rtpToWS uses 100ms read deadline loop to detect ctx cancellation without blocking forever"
    - "wsutil.ReadServerData returns ([]byte, ws.OpCode, error) — single frame per call; text-frame filter applied before JSON unmarshal"

key-files:
  created:
    - "internal/bridge/manager.go — PortPool struct, NewPortPool, Acquire, Release; CallManager struct, NewCallManager, AcquirePort, ReleasePort, StartSession"
    - "internal/bridge/manager_test.go — 5 TDD tests: three-port pre-load, non-blocking exhaustion, release-reuse, invalid range, concurrent acquire no-race"
    - "internal/bridge/session.go — CallSession struct; run() with Pitfall-6 guard, RTP socket, WS connect, wg.Add(2)/goroutines/<-ctx.Done()/wg.Wait()/sendStop; rtpToWS; wsToRTP"
    - "internal/bridge/ws.go — stub: dialWS, sendConnected, sendStart, sendStop, writeJSON (TODO: 06-03)"
  modified: []

key-decisions:
  - "wsutil.ReadServerData returns ([]byte, ws.OpCode, error) not a slice — ws.go stub's wsutil usage corrected during build (Rule 3 auto-fix)"
  - "wg.Wait() called BEFORE sendStop in run() — rtpToWS is sole WS writer during audio forwarding; sendStop only runs after rtpToWS exits; prevents concurrent write race on gobwas/ws connection"
  - "Pitfall-6 guard: ctx.Err() != nil check at run() entry handles race where BYE arrives before session goroutine starts"
  - "Port release via defer m.portPool.Release(rtpPort) in StartSession — CON-02: port always returned even if session exits early"

patterns-established:
  - "Pattern: All session cleanup in defers (rtpConn.Close, wsConn.Close, sessions.Delete, portPool.Release) — guaranteed on every exit path"
  - "Pattern: SessionCtx derived from dlg.Context() — dialog BYE auto-propagates cancellation to rtpToWS/wsToRTP goroutines"
  - "Pattern: rtpToWS sole WS writer during audio; wsToRTP sole WS reader; run() writes sendStop only post-wg.Wait() — strict single-writer-at-a-time throughout session lifetime"

requirements-completed: [CON-01, CON-02, WSR-01]

# Metrics
duration: 3min
completed: 2026-03-04
---

# Phase 6 Plan 02: RTP Port Pool + CallSession Lifecycle Summary

**Channel-based PortPool with TDD (5 tests, -race) + CallSession goroutine lifecycle with WS write-safety guarantee (wg.Wait before sendStop) + ws.go stub for 06-03**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-04T07:04:45Z
- **Completed:** 2026-03-04T07:07:46Z
- **Tasks:** 2 (Task 1 TDD has 2 commits: RED + GREEN)
- **Files modified:** 4 created

## Accomplishments

- TDD cycle complete: 5 RED tests → manager.go stub (compile fail confirmed) → GREEN implementation → all pass with -race
- PortPool uses buffered `chan int`: non-blocking `select default` returns error immediately on exhaustion (SIP-04); no mutexes needed
- CallManager satisfies `sip.CallManagerIface` (AcquirePort/ReleasePort/StartSession) — no circular import with sip package
- CallSession.run() implements full Twilio Media Streams lifecycle: RTP socket bind, WS dial, sendConnected/sendStart, bidirectional goroutines, wg.Wait() drain, sendStop
- Write-safety invariant enforced: rtpToWS is sole WS writer during audio; sendStop writes only after wg.Wait() confirms rtpToWS exited
- ws.go stub compiles session.go without implementation — 06-03 replaces with full gobwas/ws implementation

## Task Commits

Each task was committed atomically:

1. **Task 1 RED: Failing TDD tests for PortPool** - `c433353` (test)
2. **Task 1 GREEN: PortPool + CallManager implementation** - `8c519fa` (feat)
3. **Task 2: CallSession lifecycle + ws.go stub** - `765811c` (feat)

**Plan metadata:** _(docs commit follows)_

_Note: TDD Task 1 has two commits (RED test → GREEN implementation)_

## Files Created/Modified

- `internal/bridge/manager.go` - PortPool (buffered-channel-based pool, 3 methods); CallManager (sync.Map registry, satisfies CallManagerIface, StartSession wraps session.run())
- `internal/bridge/manager_test.go` - 5 TDD tests: three-port pre-load, non-blocking exhaustion, release-reuse, invalid range error, concurrent acquire no-race
- `internal/bridge/session.go` - CallSession struct (callID, streamSid, dlg, rtpPort, callerRTP, dtmfPT, cfg, log, cancel, wg); run() with Pitfall-6 guard + full lifecycle; rtpToWS (100ms deadline loop, DTMF drop, base64 PCMU encode); wsToRTP (Twilio event dispatch: media→RTP, stop→BYE+cancel)
- `internal/bridge/ws.go` - Stubs for dialWS, sendConnected, sendStart, sendStop, writeJSON — all return nil; replaced in 06-03

## Decisions Made

- **wsutil.ReadServerData API correction:** gobwas/ws `wsutil.ReadServerData` returns `([]byte, ws.OpCode, error)` — single frame, not a slice of messages. The plan referenced a slice-based API that does not exist. Fixed during build (Rule 3: blocking issue). ws.OpCode filter added to skip binary frames.
- **wg.Wait() before sendStop:** RESEARCH Pitfall 5 — gobwas/ws is not concurrent-write-safe. Enforced single-writer-at-a-time by having rtpToWS own wsConn writes during audio forwarding and run() own sendStop only after wg.Wait() drains rtpToWS.
- **Pitfall-6 guard in run():** ctx.Err() != nil check at entry prevents panic/deadlock when BYE arrives between onInvite goroutine launch and session.run() start.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] wsutil.ReadServerData returns single frame, not slice**
- **Found during:** Task 2 (session.go build)
- **Issue:** Plan's wsToRTP used `wsutil.ReadServerData` as if it returned a slice of messages. Actual signature: `([]byte, ws.OpCode, error)` — one frame per call. Build failed with "assignment mismatch: 2 variables but wsutil.ReadServerData returns 3 values".
- **Fix:** Changed to single-frame return pattern, added `ws.OpCode` variable, applied text-frame filter (`op != ws.OpText`), restructured message processing loop accordingly.
- **Files modified:** `internal/bridge/session.go`
- **Verification:** `go build ./internal/bridge/...` succeeds; `go vet` clean
- **Committed in:** `765811c` (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking API mismatch)
**Impact on plan:** Required fix to compile. Correct behavior — single-frame read with opcode check is proper gobwas/ws usage pattern. No scope creep.

## Issues Encountered

- gobwas/ws `wsutil.ReadServerData` API differs from plan's implicit assumption of slice-based API. Resolved immediately as Rule 3 auto-fix.

## User Setup Required

None - no external service configuration required for this phase.

## Next Phase Readiness

- `internal/bridge/manager.go` exports `PortPool`, `NewPortPool`, `CallManager`, `NewCallManager` — ready for main.go wiring in 06-03
- `CallManager` satisfies `sip.CallManagerIface` — can be passed to `sip.NewHandler()` immediately
- `ws.go` stubs ensure bridge package compiles; 06-03 replaces each stub with gobwas/ws implementation
- Port pool initialization: `NewPortPool(cfg.RTPPortMin, cfg.RTPPortMax)` → returns `*PortPool` or error

---
*Phase: 06-inbound-call-rtp-bridge*
*Completed: 2026-03-04*
