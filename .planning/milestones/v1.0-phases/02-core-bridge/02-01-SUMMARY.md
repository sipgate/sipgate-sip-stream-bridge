---
phase: 02-core-bridge
plan: 01
subsystem: rtp
tags: [rtp, sdp, dgram, udp, pcmu, dtmf, rfc3550, rfc4733, eventemitter]

# Dependency graph
requires:
  - phase: 01-foundation
    provides: Config type (RTP_PORT_MIN, RTP_PORT_MAX, SDP_CONTACT_IP) and createChildLogger
provides:
  - parseSdpOffer — extracts remoteIp, remotePort, hasPcmu from SIP INVITE SDP body
  - buildSdpAnswer — produces CRLF SDP string advertising PCMU+telephone-event
  - createRtpHandler — async factory; allocates port from range, returns EventEmitter RTP socket wrapper
  - RtpHandler interface — startForwarding, setRemote, sendAudio, dispose + audio/dtmf events
affects: [02-04-call-manager, 02-02-ws-client]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Module-level port counter for synchronous sequential UDP port allocation"
    - "EventEmitter subclass pattern for per-call RTP socket wrapping"
    - "Pure-function SDP module (no side effects, no I/O)"
    - "EADDRINUSE retry loop with bounded attempt count"

key-files:
  created:
    - src/sip/sdp.ts
    - src/rtp/rtpHandler.ts
  modified: []

key-decisions:
  - "as number assertion on module-level nextPort counter — TypeScript cannot narrow mutable module variables through a guard, so an explicit cast is used"
  - "Extension-header-aware RTP parser — CC and X bits handled per RFC 3550 so exotic RTP senders do not crash the handler"
  - "dispose() swallows socket.close() errors — socket may already be closed (ENOTCONN); silently ignoring is correct"
  - "DTMF emit only on End=true — avoids duplicate digit events from repeated telephone-event packets"

patterns-established:
  - "RTP socket per call: never share a dgram socket across calls"
  - "Port counter wraps within [portMin, portMax] and warns when fewer than 10 remain"

requirements-completed: [SIP-03, WSB-03, WSB-05, WSB-07]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 02 Plan 01: SDP/RTP Primitives Summary

**Pure SDP offer/answer functions and per-call RTP dgram socket wrapper with RFC 3550 header parsing, PCMU/DTMF event emission, and sequential port allocation**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-03T13:08:15Z
- **Completed:** 2026-03-03T13:10:19Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments

- `parseSdpOffer` extracts remote IP, port and PCMU presence from any SDP body; returns null for malformed input
- `buildSdpAnswer` produces RFC 4566 compliant CRLF SDP advertising PCMU + telephone-event with correct attributes
- `createRtpHandler` binds a dgram udp4 socket from a module-level sequential counter, retries on EADDRINUSE, warns near exhaustion
- RTP message handler is CC-count and extension-header-aware per RFC 3550; emits `audio` on PT=0, `dtmf` (End-only) on PT=101

## Task Commits

Each task was committed atomically:

1. **Task 1: SDP offer parser and answer builder** - `e3216a3` (feat)
2. **Task 2: Per-call RTP handler** - `d18d2a5` (feat)

## Files Created/Modified

- `src/sip/sdp.ts` — Pure-function SDP parsing and answer generation; exports SdpOffer, parseSdpOffer, buildSdpAnswer
- `src/rtp/rtpHandler.ts` — RtpHandlerImpl extends EventEmitter; exports createRtpHandler factory and RtpHandler interface

## Decisions Made

- Used `as number` assertion on `nextPort` module variable — TypeScript cannot narrow mutable module-level variables through an `if` guard, explicit cast is the minimal correct fix.
- Included CC and extension-header parsing in `onMessage` — exotic RTP senders may set these bits; ignoring them causes incorrect payload extraction and silent data corruption.
- `dispose()` swallows `socket.close()` errors — if CallManager calls dispose twice or socket is already ENOTCONN, throwing would propagate an unhandled rejection where none is warranted.
- DTMF emitted only on End=true — telephone-event is retransmitted multiple times per digit; emitting on every packet would send 4–8 duplicate DTMF events to the WebSocket consumer.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] TypeScript error on module-level port counter narrowing**
- **Found during:** Task 2 (per-call RTP handler)
- **Issue:** `const port = nextPort` inside the for-loop gave TS7022 "implicitly has type 'any'" because TypeScript cannot narrow a mutable `number | undefined` module variable past the guard
- **Fix:** Added explicit `const port: number = nextPort as number` — semantically safe because the guard immediately above guarantees `nextPort` is defined
- **Files modified:** src/rtp/rtpHandler.ts
- **Verification:** `pnpm typecheck` exits 0
- **Committed in:** d18d2a5 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (Rule 1 - TypeScript narrowing bug)
**Impact on plan:** Required for compilation; zero semantic change.

## Issues Encountered

None beyond the TypeScript narrowing fix above.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- `src/sip/sdp.ts` and `src/rtp/rtpHandler.ts` are ready for import by CallManager (Plan 04)
- Both files pass `pnpm typecheck` and `pnpm build` with zero errors
- No external dependencies added — only Node built-ins (`node:dgram`, `node:crypto`, `node:events`)

---
*Phase: 02-core-bridge*
*Completed: 2026-03-03*

## Self-Check: PASSED

- src/sip/sdp.ts: FOUND
- src/rtp/rtpHandler.ts: FOUND
- 02-01-SUMMARY.md: FOUND
- commit e3216a3 (Task 1): FOUND
- commit d18d2a5 (Task 2): FOUND
