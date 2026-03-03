---
phase: 02-core-bridge
plan: "04"
subsystem: bridge
tags: [sip, rtp, websocket, callmanager, twilio-media-streams, pcmu]

# Dependency graph
requires:
  - phase: 02-01
    provides: parseSdpOffer, buildSdpAnswer, createRtpHandler (RTP primitives)
  - phase: 02-02
    provides: createWsClient, WsClient interface (WebSocket client)
  - phase: 02-03
    provides: createSipUserAgent, SipHandle, SipCallbacks (SIP user agent)
provides:
  - CallManager class orchestrating inbound SIP INVITE lifecycle
  - CallSession type capturing full per-call dialog state
  - Bidirectional audio bridge between RTP and WebSocket backend
  - Graceful terminateAll() for shutdown
affects: [03-wiring, 04-testing, src/index.ts]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "INVITE handler sequence: 100 Trying → 488/503 on failure | 180 Ringing → 200 OK with SDP"
    - "sendBye flag controls unilateral vs symmetric BYE — prevents protocol violation on remote-initiated BYE"
    - "terminateSession idempotency via Map.has() guard before delete"
    - "terminateAll re-inserts sessions before delegating to terminateSession (avoids duplicated logic)"
    - "buildBye routing uses Contact URI from INVITE, not rinfo.address (RFC 3261 §12.2)"

key-files:
  created:
    - src/bridge/callManager.ts
  modified: []

key-decisions:
  - "SDP_CONTACT_IP ?? '127.0.0.1' fallback in terminateSession — safe default when config has no explicit IP"
  - "callLog created before RTP allocation so all per-call events share consistent bindings"
  - "onDisconnect wired after session stored — ensures terminateSession finds the session in the Map"
  - "sendBye=false path for remote BYE — 200 OK to BYE already sent by handleBye; outbound BYE would be RFC violation"

patterns-established:
  - "CallManager.getCallbacks() decouples SIP event routing from SIP socket implementation"
  - "Per-call logger with callId + streamSid bindings on every log event"

requirements-completed: [SIP-03, SIP-04, SIP-05, WSB-01, WSB-02, WSB-03, WSB-04, WSB-05, CON-01, CON-02]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 02 Plan 04: CallManager Summary

**CallManager class wiring RTP + WS + SIP into a bidirectional audio bridge with per-call session state and clean BYE/disconnect teardown**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-03T13:13:02Z
- **Completed:** 2026-03-03T13:14:44Z
- **Tasks:** 1
- **Files modified:** 1

## Accomplishments
- CallSession interface with all dialog state (callId, callSid, streamSid, rtp, ws, localTag, remoteTag, remoteUri, remoteTarget, cseq, log)
- CallManager class with setSipHandle(), getCallbacks(), terminateAll()
- Full INVITE lifecycle: 100 Trying → SDP check (488) | RTP alloc + WS connect (503) | 180 Ringing → 200 OK with SDP answer
- Bidirectional audio bridge: rtp 'audio' events forwarded to ws.sendAudio; ws.onAudio events forwarded to rtp.sendAudio
- DTMF forwarding: rtp 'dtmf' events forwarded to ws.sendDtmf
- BYE from remote: 200 OK sent, terminateSession(sendBye=false) — no outbound BYE
- WS disconnect: terminateSession(sendBye=true) — SIP BYE sent to caller via Contact URI
- terminateAll() reuses terminateSession(sendBye=true) for clean parallel shutdown

## Task Commits

Each task was committed atomically:

1. **Task 1: CallSession type and CallManager class with INVITE lifecycle** - `43f4c64` (feat)

**Plan metadata:** (docs commit — see below)

## Files Created/Modified
- `src/bridge/callManager.ts` - Central coordinator: CallManager class + CallSession type, full INVITE lifecycle, bidirectional audio bridge, BYE/disconnect teardown, buildBye/parseContactTarget helpers

## Decisions Made
- SDP_CONTACT_IP falls back to '127.0.0.1' in terminateSession — safe default matching the outbound SIP socket IP logic
- callLog is created before RTP allocation (step 4 in plan order) so all per-call log events have consistent callId binding from the start
- onDisconnect handler registered after session is stored in Map — ensures terminateSession finds the session and doesn't silently no-op
- sendBye=false on remote-initiated BYE path — handleBye already sent 200 OK to the caller's BYE; sending another BYE would be a protocol violation per RFC 3261

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- CallManager is complete and ready to be wired into src/index.ts
- getSipCallbacks() must be called before createSipUserAgent so the handle can be passed to setSipHandle() immediately after
- All Phase 2 plans (01–04) are complete; Phase 3 can wire the full pipeline

---
*Phase: 02-core-bridge*
*Completed: 2026-03-03*
