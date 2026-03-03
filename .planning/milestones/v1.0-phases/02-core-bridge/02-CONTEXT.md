# Phase 2: Core Bridge - Context

**Gathered:** 2026-03-03
**Status:** Ready for planning

<domain>
## Phase Boundary

An inbound SIP INVITE from sipgate triggers a WebSocket connection to the configured backend, and bidirectional audio flows: caller RTP → base64 mulaw `media` events over WebSocket, and WebSocket `media` events → outbound RTP back to caller. Multiple concurrent calls are supported with independent WebSocket connections and RTP sockets. Outbound calls, SRTP, transcoding, and WebSocket resilience (reconnect) are out of scope for this phase.

</domain>

<decisions>
## Implementation Decisions

### Pre-answer ringing behavior
- Send `100 Trying` → `180 Ringing` → attempt WebSocket connection → `200 OK` or `503`
- WebSocket connect timeout: **2 seconds** — send 503 if WS not connected within 2s
- Forward audio immediately after WS `open` event — no application-level handshake wait
- RTP audio arriving during the 180 Ringing / WS connect window is **dropped** (not buffered)

### DTMF transport
- RFC 2833 / RFC 4733 telephone-event carried in RTP stream (payload type 101)
- SDP answer includes `a=rtpmap:101 telephone-event/8000` and `a=fmtp:101 0-16`
- Emit one `dtmf` event per keypress — triggered on the RTP telephone-event packet with `End=true`
- **PCMU only** — SDP answer offers only PCMU (payload type 0); if caller offers no PCMU, send `488 Not Acceptable Here`

### Call & Stream ID format
- `callSid`: `CA` + 32 random hex chars (Twilio-style, e.g. `CA3a2b1c...`)
- `streamSid`: `MZ` + 32 random hex chars (Twilio-style, e.g. `MZf4e3d2...`)
- SIP Call-ID is included as `sipCallId` in `start.customParameters` (alongside `From`, `To`, `Call-ID`) for sipgate log correlation
- Both IDs are generated fresh per call at INVITE time

### Graceful shutdown
- Handle both **SIGTERM** and **SIGINT** with the same handler (production + dev Ctrl+C)
- Sequence: send SIP BYE to all active calls **and** close their WebSocket connections **in parallel** → then send SIP UNREGISTER
- **5 second drain timeout**: if cleanup is not complete in 5s, force `process.exit(0)`
- Each active call's `stop` event is sent to the WebSocket before closing the WS connection

### Claude's Discretion
- RTP port pool implementation details (counter vs pool, RTCP port handling)
- Exact SDP body format (as long as PCMU + telephone-event are correctly advertised)
- Internal CallSession data structure
- Error logging verbosity for individual RTP packet drops

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- `src/logger/index.ts` — `createChildLogger({component, callId?, streamSid?})`: use this to create per-call loggers with `callId` and `streamSid` bound at CallSession creation time
- `src/config/index.ts` — `Config` type with `WS_TARGET_URL`, `RTP_PORT_MIN`, `RTP_PORT_MAX`, `SDP_CONTACT_IP` already validated
- `src/sip/userAgent.ts` — existing UDP socket on port 5060 receives all SIP messages; Phase 2 extends its `message` handler to dispatch INVITE/BYE/ACK to the CallManager (the socket is owned here)

### Established Patterns
- Raw `dgram` UDP socket for all SIP traffic — no SIP.js; manual SIP message construction used in Phase 1 for REGISTER; same pattern extends to INVITE/BYE/ACK in Phase 2
- `ws` package already in `dependencies` — use `WebSocket` from `ws` for the per-call WS client
- pino child logger per call with `callId` + `streamSid` bound fields (OBS-01 pattern already established)
- Domain-layered source: Phase 2 adds `src/rtp/` (RTP handler) and `src/ws/` (WS client) alongside existing `src/sip/`, `src/config/`, `src/logger/`

### Integration Points
- `src/index.ts` — comment `// Phase 2 will add INVITE handlers here` is the wiring point; CallManager is instantiated here and passed to `createSipUserAgent`
- `SipHandle` interface in `src/sip/userAgent.ts` — will need extension (or a new callback param) to route INVITE/BYE/ACK/OPTIONS from the socket's `message` handler to CallManager
- `Config.SDP_CONTACT_IP` — used in SDP `c=` and `o=` lines for the RTP contact address; falls back to `getLocalIp()` (same pattern as Phase 1's `viaHost`)
- `network_mode: host` in docker-compose.yml — RTP sockets bind directly to host network; no port mapping needed on Linux production

### Known Concerns from Research
- macOS development: `network_mode: host` is Linux-only; for local RTP testing, either use a docker-compose.override.yml with explicit port range publishing or test directly with `pnpm dev` (no Docker)
- SIP re-REGISTER timer in Phase 1 uses `setTimeout` — ensure SIGTERM handler cancels this timer to avoid callbacks after shutdown

</code_context>

<specifics>
## Specific Ideas

- Twilio Media Streams wire format is the locked choice — `connected`, `start`, `media`, `stop`, `dtmf` event types with Twilio-compatible field names
- `start.customParameters` must include: `From`, `To`, `sipCallId` (SIP Call-ID value) — backends use these for call context
- CA.../MZ... ID format chosen for drop-in compatibility with AI voice backends built against Twilio

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 02-core-bridge*
*Context gathered: 2026-03-03*
