# Phase 3: Resilience - Context

**Gathered:** 2026-03-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Replace the current "WS disconnect → send BYE immediately" behavior with a reconnect loop that keeps the SIP call alive during transient WebSocket drops. The RTP socket and SIP dialog remain open throughout. Initial connection failure at call setup (no WS within 2s) still returns 503 as before — reconnect only applies to mid-call drops.

Also verify FD cleanup: after 20 sequential calls, the process FD count returns to baseline.

</domain>

<decisions>
## Implementation Decisions

### Retry budget
- Total reconnect window: **30 seconds** — if WS is not reconnected within 30s, give up and send SIP BYE
- Exponential backoff: 1s → 2s → 4s → 4s → 4s... (cap at **4 seconds** per attempt)
- On giving up: `terminateSession(session, 'ws_reconnect_failed', true)` — sends BYE, stop event not possible (WS gone)

### Stream identity after reconnect
- **Same `streamSid` and `callSid`** in the fresh `connected` + `start` events after reconnect
- Backend can detect the gap (sequence number resets or timestamp jump) and continue the conversation
- The `connected` event is sent first, then `start`, before any `media` events resume (per WSR-02)

### Caller experience during reconnect
- **Send RTP silence** (0xFF μ-law, 160 bytes per 20ms packet) to the caller throughout the reconnect window
- Prevents dead-air / phone comfort-noise / call-quality warnings on the caller side
- Silence stops as soon as WS reconnects and first `media` event arrives from backend
- Inbound RTP from caller is **dropped** during reconnect window — not buffered (WSR-03, consistent with Phase 2 "drop not buffer" pattern)

### Claude's Discretion
- Exact timer/interval implementation (setTimeout loop vs recursive retry)
- Whether the silence sender is a separate setInterval or integrated into the reconnect loop
- FD leak test implementation details (how to count open FDs on Linux/macOS)

</decisions>

<code_context>
## Existing Code Insights

### Reusable Assets
- `src/ws/wsClient.ts` — `createWsClient(url, params, log)`: the reconnect loop will call this again with the same `params` (same streamSid/callSid); the `WsClient` interface can be reused as-is
- `src/bridge/callManager.ts:468` — `ws.onDisconnect(() => this.terminateSession(...))`: this is the exact line Phase 3 replaces with a reconnect handler
- `src/rtp/rtpHandler.ts` — `rtp.sendAudio(payload)`: use this to send silence packets during reconnect window; `Buffer.alloc(160, 0xff)` is μ-law silence

### Established Patterns
- "Drop not buffer" during setup windows (Phase 2 context) — same policy applies during reconnect
- `createChildLogger` per call with `callId` bound — reconnect log entries should use the same per-call logger
- `callLog.info({ event: 'ws_reconnect_attempt', attempt, delay })` — follow existing structured log pattern

### Integration Points
- `callManager.ts` `handleInvite`: after wiring `ws.onDisconnect`, Phase 3 replaces the lambda body — instead of calling `terminateSession`, it starts a reconnect loop using `session.rtp`, `session.callId`, and the original `params`
- `CallSession` interface may need a `wsReconnecting: boolean` flag to suppress `sendAudio` warn logs during the reconnect window
- `rtp.startForwarding()` stays called once — inbound RTP just gets dropped (no forwarding) until the new WsClient is wired

</code_context>

<specifics>
## Specific Ideas

- The 30s / 4s-cap backoff means roughly: 1s, 2s, 4s, 4s, 4s, 4s, 4s, 4s = ~30s total across ~8 attempts
- Silence is already used for NAT hole-punch (`Buffer.alloc(160, 0xff)`), same buffer reusable here
- After reconnect, sequence numbers restart from seq=2 (same as initial connect) — backend sees a fresh stream with the same SID

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 03-resilience*
*Context gathered: 2026-03-03*
