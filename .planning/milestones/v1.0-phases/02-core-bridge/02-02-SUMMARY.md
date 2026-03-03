---
phase: 02-core-bridge
plan: "02"
subsystem: websocket
tags: [ws, twilio-media-streams, websocket, rtp, mulaw, dtmf]

# Dependency graph
requires:
  - phase: 01-foundation
    provides: createChildLogger, Config type with WS_TARGET_URL

provides:
  - Per-call WebSocket client implementing Twilio Media Streams wire protocol
  - WsClient interface with sendAudio, sendDtmf, stop, onAudio, onDisconnect
  - createWsClient factory with 2-second connect timeout guard
  - WsCallParams type for call metadata

affects:
  - 02-03-rtp (RTP handler calls sendAudio, registers onAudio)
  - 02-04-call-manager (CallManager orchestrates createWsClient with RTP)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Twilio Media Streams wire format: connected → start → media → stop → dtmf events as JSON over WebSocket"
    - "Per-call WebSocket: fresh WebSocket instance per call via createWsClient factory (no sharing between calls)"
    - "Connect timeout guard: 2s setTimeout + ws.terminate on expiry, clearTimeout on open/error"
    - "Drop-silent on not-OPEN: sendAudio and sendDtmf check readyState before send, log.warn and return on miss"
    - "Monotonic sequence counter: seq starts at 2 (1 used by start event), rtpTimestamp += 160 per packet"

key-files:
  created:
    - src/ws/wsClient.ts
  modified: []

key-decisions:
  - "rtpTimestamp increments by 160 per sendAudio call (20ms at 8kHz PCMU) — matches RTP clock rate"
  - "stop() calls ws.close() (graceful) when OPEN, ws.terminate() otherwise — avoids hanging sockets"
  - "onAudio ignores non-media events silently (mark, clear, etc.) — forward-compatible with protocol extensions"

patterns-established:
  - "Twilio Media Streams JSON envelope pattern: {event, sequenceNumber, <event-name-key>, streamSid}"
  - "WS client lifecycle: createWsClient (Promise) → onAudio/onDisconnect registration → sendAudio/sendDtmf → stop()"

requirements-completed: [WSB-01, WSB-02, WSB-04, WSB-05, WSB-06, WSB-07]

# Metrics
duration: 1min
completed: 2026-03-03
---

# Phase 2 Plan 02: WS Client Summary

**Per-call WebSocket client wrapping Twilio Media Streams protocol — connected/start/media/stop/dtmf events with 2-second connect timeout and base64 mulaw audio bridging**

## Performance

- **Duration:** 1 min
- **Started:** 2026-03-03T13:08:12Z
- **Completed:** 2026-03-03T13:09:03Z
- **Tasks:** 1
- **Files modified:** 1

## Accomplishments
- Created `src/ws/wsClient.ts` exporting `WsCallParams`, `WsClient`, and `createWsClient`
- `createWsClient` connects to the backend URL with a 2-second timeout, rejecting the returned Promise if the WebSocket does not open in time
- Sends `connected` then `start` events synchronously after WebSocket `open` fires; `start` includes `customParameters` with `From`, `To`, and `sipCallId` for backend call correlation
- `sendAudio` forwards inbound RTP mulaw payloads as Twilio `media` events (base64-encoded), with monotonic `seq` and `chunk` counters and `rtpTimestamp` advancing 160 per packet
- `sendDtmf` emits Twilio `dtmf` events; both send methods silently drop packets when the socket is not `OPEN`
- `stop` sends the Twilio `stop` event then calls `ws.close()` (graceful); falls back to `ws.terminate()` if not `OPEN`
- `onAudio` decodes `media` events from the backend (base64 → Buffer), ignores non-media messages
- `onDisconnect` wires both `close` and `error` events to a single handler

## Task Commits

Each task was committed atomically:

1. **Task 1: WS client with Twilio Media Streams protocol and 2-second connect timeout** - `1db58d7` (feat)

**Plan metadata:** (docs commit follows)

## Files Created/Modified
- `src/ws/wsClient.ts` - Per-call WebSocket client factory implementing the Twilio Media Streams wire protocol

## Decisions Made
- `rtpTimestamp` increments by 160 per `sendAudio` call (20ms × 8000Hz = 160 samples) — correct RTP clock for PCMU
- `stop()` calls `ws.close()` when `readyState === OPEN` (sends a proper WS close frame) and `ws.terminate()` otherwise to avoid hanging sockets
- `onAudio` silently ignores non-media events (mark, clear, etc.) to remain forward-compatible with Twilio protocol extensions

## Deviations from Plan

None — plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None — no external service configuration required.

## Next Phase Readiness
- `WsClient` is ready for Plan 02-03 (RTP handler) to call `sendAudio` on incoming RTP packets and register `onAudio` for outbound RTP
- `createWsClient` is ready for Plan 02-04 (CallManager) to wire INVITE handling → WS connect → audio flow
- No blockers for subsequent wave plans

---
*Phase: 02-core-bridge*
*Completed: 2026-03-03*
