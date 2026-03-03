---
phase: 02-core-bridge
plan: "03"
subsystem: sip
tags: [sip, dgram, udp, callbacks, rfc3261, typescript]

# Dependency graph
requires:
  - phase: 01-foundation
    provides: createSipUserAgent with REGISTER auth flow, SipHandle.stop()
provides:
  - SipCallbacks interface (onInvite, onAck, onBye) injected into createSipUserAgent
  - SipHandle.sendRaw(buf, port, host) for CallManager to send SIP responses/BYE
  - SipHandle.unregister() — REGISTER Expires:0 Contact:* for graceful shutdown
  - Inbound INVITE/ACK/BYE dispatch to callbacks
  - Automatic 200 OK response to OPTIONS keepalive probes
affects:
  - 02-04-call-manager (consumes SipCallbacks + sendRaw + unregister)
  - 02-05-websocket (indirect, via CallManager)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - Dependency-injection callbacks pattern for SIP dispatch (avoids tight coupling to CallManager)
    - Fire-and-forget sendRaw with error logging (consistent with existing sendRegister pattern)
    - RFC 3261 §10.2.2 wildcard Contact:* deregistration in unregister()

key-files:
  created: []
  modified:
    - src/sip/userAgent.ts

key-decisions:
  - "SipCallbacks optional on createSipUserAgent — backward compatible, existing call sites with 2 args still compile"
  - "unregister() is fire-and-forget (Promise.resolve()) — shutdown drain timeout covers the response window"
  - "OPTIONS auto-responded inline (no callback) — keepalive probes are infrastructure, not call logic"

patterns-established:
  - "SipCallbacks|onInvite|onAck|onBye — pattern used by CallManager to wire call lifecycle"

requirements-completed: [SIP-03, SIP-04, SIP-05]

# Metrics
duration: 1min
completed: 2026-03-03
---

# Phase 2 Plan 03: SIP Inbound Dispatch Summary

**Raw UDP socket extended with SipCallbacks injection, sendRaw/unregister on SipHandle, and automatic OPTIONS 200 OK — CallManager hook points fully wired**

## Performance

- **Duration:** 1 min
- **Started:** 2026-03-03T13:08:11Z
- **Completed:** 2026-03-03T13:09:42Z
- **Tasks:** 1
- **Files modified:** 1

## Accomplishments

- Added `SipCallbacks` interface (`onInvite`, `onAck`, `onBye`) with `RemoteInfo` parameter — exact hook points CallManager needs
- Extended `SipHandle` with `sendRaw(buf, port, host)` and `unregister(): Promise<void>` while preserving existing `stop()`
- Updated `createSipUserAgent` to accept optional third parameter `callbacks?: SipCallbacks` — fully backward compatible
- Message handler now dispatches INVITE/ACK/BYE to respective callbacks and auto-responds 200 OK to OPTIONS probes
- All existing REGISTER auth flow (401/407 challenge-response), re-registration timer, and `stop()` logic preserved exactly

## Task Commits

Each task was committed atomically:

1. **Task 1: Add SipCallbacks type, extend SipHandle, extend message handler for INVITE/ACK/BYE/OPTIONS dispatch** - `bb0f782` (feat)

## Files Created/Modified

- `src/sip/userAgent.ts` — Added SipCallbacks interface, extended SipHandle with sendRaw/unregister, updated message handler to dispatch INVITE/ACK/BYE and auto-respond to OPTIONS

## Decisions Made

- `SipCallbacks` is optional on `createSipUserAgent` — backward compatible with 2-argument call sites (existing `src/index.ts` continues to compile unchanged)
- `unregister()` is fire-and-forget (`Promise.resolve()` immediately) — shutdown drain timeout handles the response window; no need to await 200 OK
- OPTIONS response is handled inline without a callback — keepalive/liveness probes are transport-layer concerns, not call logic; no CallManager involvement required

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

Pre-existing TypeScript errors in `src/rtp/rtpHandler.ts` (another plan in progress) are out of scope — they existed before this plan and `src/sip/userAgent.ts` has no errors.

## Next Phase Readiness

- `SipHandle` is now the full interface CallManager (Plan 04) needs: `sendRaw` for sending SIP responses, `unregister` for graceful shutdown, and `SipCallbacks` for receiving inbound call events
- `createSipUserAgent(config, log, callbacks)` is the wiring point — Plan 04 just needs to pass its callback implementations
- No blockers for Plan 04

---
*Phase: 02-core-bridge*
*Completed: 2026-03-03*

## Self-Check: PASSED

- src/sip/userAgent.ts: FOUND
- 02-03-SUMMARY.md: FOUND
- Commit bb0f782: FOUND
