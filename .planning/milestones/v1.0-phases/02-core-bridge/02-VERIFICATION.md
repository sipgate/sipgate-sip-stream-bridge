---
phase: 02-core-bridge
verified: 2026-03-03T00:00:00Z
status: human_needed
score: 6/6 must-haves verified
human_verification:
  - test: "Make an inbound SIP test call and observe connected + start on the WS backend"
    expected: "connected event arrives first, then start event with correct streamSid (MZ...), callSid (CA...), From, To, sipCallId in customParameters — before any media events"
    why_human: "Requires a live SIP call from sipgate and a WebSocket listener; cannot verify actual wire-format ordering programmatically against a real SIP trunk"
  - test: "Confirm caller audio arrives at WS backend as base64 mulaw media events"
    expected: "media events contain base64-encoded PCMU payload with track='inbound', and decoding yields valid 8kHz audio"
    why_human: "RTP decoding and base64 encoding correctness cannot be verified without a live RTP packet stream"
  - test: "Confirm audio sent from WS backend plays back audibly to the caller"
    expected: "Outbound RTP packets with correct 12-byte header and PCMU payload are received at the caller"
    why_human: "Requires a live call and a WebSocket client injecting audio; cannot observe actual RTP transmission programmatically"
  - test: "Place a call to an unreachable WS backend and verify the caller receives SIP 503"
    expected: "Caller hears busy/unavailable; SIP 503 is returned within ~2 seconds of the INVITE"
    why_human: "Requires live SIP call with WS_TARGET_URL pointing to a closed port; timeout enforcement is runtime behavior"
  - test: "Place two simultaneous test calls and verify they are independent"
    expected: "Each call has a distinct streamSid; audio from call A does not appear on call B's WS connection"
    why_human: "Requires two concurrent SIP calls and two WebSocket listeners to verify stream isolation"
  - test: "Send SIGTERM while a call is active and verify BYE + stop event + UNREGISTER sequence"
    expected: "Caller receives SIP BYE; WS backend receives stop event; service logs shutdown_complete and exits cleanly within 5 seconds"
    why_human: "Requires a live call and real signal delivery to verify the drain sequence; runtime behaviour only"
---

# Phase 2: Core Bridge Verification Report

**Phase Goal:** An inbound SIP call from sipgate reaches the WebSocket backend as Twilio Media Streams events, and audio from the backend plays back to the caller — multiple concurrent calls work independently
**Verified:** 2026-03-03
**Status:** human_needed (all automated checks passed; 6 integration items require live-call testing)
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| #  | Truth                                                                                                                        | Status     | Evidence                                                                                                                                   |
|----|------------------------------------------------------------------------------------------------------------------------------|------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| 1  | Inbound SIP INVITE triggers connected + start events on WS backend with correct metadata before any media events             | ? HUMAN    | wsClient.ts lines 44–75: connected + start sent synchronously on 'open'; start includes customParameters(From, To, sipCallId), streamSid   |
| 2  | Caller audio arrives at WS backend as base64 mulaw media events                                                              | ? HUMAN    | callManager.ts line 330: `rtp.on('audio', (p) => ws.sendAudio(p))` wired; wsClient.ts line 97–110: base64 encodes payload in media event   |
| 3  | Audio from WS backend plays back to caller via RTP                                                                           | ? HUMAN    | callManager.ts line 334: `ws.onAudio((p) => rtp.sendAudio(p))` wired; rtpHandler.ts lines 94–110: prepends 12-byte RTP header, sends UDP   |
| 4  | If WS backend is unreachable, caller receives SIP 503                                                                        | ? HUMAN    | callManager.ts lines 267–287: catch on createWsClient timeout → buildResponse 503 → sipHandle.sendRaw + rtp.dispose                       |
| 5  | Two simultaneous calls have independent streamSid values and independent WS connections; audio does not cross                 | ? HUMAN    | callManager.ts line 139: `Map<string, CallSession>` keyed by SIP Call-ID; each INVITE creates its own RtpHandler + WsClient instance      |
| 6  | After call ends, SIP BYE + stop event sent; on SIGTERM all active calls terminated and service UNREGISTERs before exiting    | ? HUMAN    | index.ts lines 38–39: terminateAll() then unregister(); terminateSession (lines 382–407): ws.stop() sends stop, rtp.dispose() closes socket |

**Score:** 6/6 truths have full code support — human testing required to confirm runtime correctness.

---

### Required Artifacts

| Artifact                        | Provides                                                        | Exists | Substantive | Wired     | Status       |
|---------------------------------|-----------------------------------------------------------------|--------|-------------|-----------|--------------|
| `src/sip/sdp.ts`                | SDP offer parsing and answer generation                         | YES    | YES (56 L)  | YES       | VERIFIED     |
| `src/rtp/rtpHandler.ts`         | Per-call RTP dgram socket, header parsing, DTMF detection       | YES    | YES (247 L) | YES       | VERIFIED     |
| `src/ws/wsClient.ts`            | Per-call WebSocket client, Twilio Media Streams protocol        | YES    | YES (175 L) | YES       | VERIFIED     |
| `src/sip/userAgent.ts`          | Extended SipHandle with sendRaw/unregister + callback dispatch  | YES    | YES (257 L) | YES       | VERIFIED     |
| `src/bridge/callManager.ts`     | CallManager class, CallSession Map, INVITE lifecycle            | YES    | YES (409 L) | YES       | VERIFIED     |
| `src/index.ts`                  | Wired entrypoint: CallManager + SipHandle + graceful shutdown   | YES    | YES (57 L)  | YES       | VERIFIED     |

No artifacts are stubs or placeholders. No anti-patterns (TODO/FIXME/console.log) found. `pnpm typecheck` exits 0. `pnpm build` produces `dist/index.js` (27.84 KB) with no errors.

---

### Key Link Verification

| From                             | To                            | Via                                             | Status  | Evidence                                                                         |
|----------------------------------|-------------------------------|-------------------------------------------------|---------|----------------------------------------------------------------------------------|
| `src/rtp/rtpHandler.ts`          | `node:dgram`                  | `dgram.createSocket('udp4')`                    | WIRED   | Line 228: `const socket = dgram.createSocket('udp4')`                            |
| `src/rtp/rtpHandler.ts`          | `src/sip/sdp.ts`              | SdpOffer type import                            | WIRED   | Line: `export interface RtpHandler` — SdpOffer used via setRemote() signature    |
| `src/ws/wsClient.ts`             | `ws` npm package              | `import { WebSocket } from 'ws'`                | WIRED   | Line 1: `import { WebSocket } from 'ws'`                                         |
| `src/ws/wsClient.ts`             | Twilio Media Streams protocol | JSON events: connected/start/media/stop/dtmf    | WIRED   | Lines 44–75, 97–148: all five event types emitted with correct structure         |
| `src/sip/userAgent.ts`           | CallManager (callbacks)       | SipCallbacks onInvite/onAck/onBye               | WIRED   | Lines 224–229: `callbacks?.onInvite?.(raw, rinfo)` etc.                          |
| `src/bridge/callManager.ts`      | `src/sip/userAgent.ts`        | SipHandle.sendRaw for all SIP responses + BYE   | WIRED   | Lines 219, 234, 263, 284, 306, 367, 401: 7 sendRaw calls                        |
| `src/bridge/callManager.ts`      | `src/ws/wsClient.ts`          | createWsClient per call                         | WIRED   | Lines 268–272: `ws = await createWsClient(config.WS_TARGET_URL, ...)`           |
| `src/bridge/callManager.ts`      | `src/rtp/rtpHandler.ts`       | createRtpHandler per call                       | WIRED   | Lines 241–245: `const rtp = await createRtpHandler({...})`                      |
| `src/bridge/callManager.ts`      | `src/sip/sdp.ts`              | parseSdpOffer + buildSdpAnswer                  | WIRED   | Lines 223, 291: both functions called in handleInvite                            |
| `ws.onDisconnect` in callManager | sipHandle.sendRaw (BYE)       | terminateSession(session, 'ws_disconnect', true) | WIRED  | Line 336: `ws.onDisconnect(() => this.terminateSession(session, 'ws_disconnect', true))` — buildBye + sendRaw at lines 389–401 |
| `src/index.ts`                   | `src/bridge/callManager.ts`   | `new CallManager` + `setSipHandle`              | WIRED   | Lines 14, 22: instantiation and wiring confirmed                                 |
| `src/index.ts`                   | `src/sip/userAgent.ts`        | `createSipUserAgent(config, log, getCallbacks())`| WIRED  | Lines 16–20: callbacks injected as third argument                                |
| `src/index.ts`                   | SIGTERM/SIGINT                | `process.on('SIGTERM/SIGINT', shutdown)`        | WIRED   | Lines 49–50: both signals wired to same async shutdown function                  |

All 13 key links verified. No broken wiring found.

---

### Requirements Coverage

| Requirement | Source Plan | Description                                                            | Status      | Evidence                                                                              |
|-------------|-------------|------------------------------------------------------------------------|-------------|---------------------------------------------------------------------------------------|
| SIP-03      | 02-01, 02-03, 02-04 | Accept inbound INVITE, negotiate PCMU                            | SATISFIED   | callManager handleInvite: SDP parse, 488 on no-PCMU, 200 OK with SDP answer          |
| SIP-04      | 02-03, 02-04 | Reject INVITE with 503 if WS backend unreachable                       | SATISFIED   | callManager lines 273–287: catch on createWsClient → 503 sent + rtp.dispose         |
| SIP-05      | 02-03, 02-04 | Send SIP BYE when call ends from either side                           | SATISFIED   | terminateSession(sendBye=true) builds+sends BYE; onBye sends 200 OK (no double-BYE) |
| WSB-01      | 02-02, 02-04 | Send connected event after WS connection established                   | SATISFIED   | wsClient.ts lines 44–50: sent synchronously in 'open' handler                        |
| WSB-02      | 02-02, 02-04 | Send start event with streamSid, callSid, tracks, mediaFormat          | SATISFIED   | wsClient.ts lines 52–75: full Twilio start payload with all required fields          |
| WSB-03      | 02-01, 02-04 | Forward inbound RTP audio as media events (base64 mulaw)               | SATISFIED   | callManager line 330 + wsClient sendAudio lines 93–110                               |
| WSB-04      | 02-02, 02-04 | Send stop event when SIP call ends                                     | SATISFIED   | wsClient.ts stop() lines 131–148: stop event sent before ws.close()                  |
| WSB-05      | 02-01, 02-04 | Receive media events from WS, convert to outbound RTP                  | SATISFIED   | callManager line 334: ws.onAudio → rtp.sendAudio; rtpHandler sendAudio lines 94–110 |
| WSB-06      | 02-02        | Call metadata (From, To, SIP Call-ID) in start.customParameters        | SATISFIED   | wsClient.ts lines 62–66: `{ From: from, To: to, sipCallId }` in customParameters    |
| WSB-07      | 02-01, 02-04 | Forward DTMF digits as dtmf events                                     | SATISFIED   | rtpHandler PT=101 emits 'dtmf'; callManager line 332 + wsClient sendDtmf            |
| CON-01      | 02-04        | Multiple simultaneous calls, each with independent WS connection        | SATISFIED   | sessions Map<string, CallSession> keyed by SIP Call-ID; each INVITE creates fresh instances |
| CON-02      | 02-04, 02-05 | Per-call RTP sockets cleaned up after call ends                        | SATISFIED   | terminateSession line 406: `session.rtp.dispose()` closes dgram socket              |
| LCY-01      | 02-05        | On SIGTERM/SIGINT: BYE all calls, UNREGISTER, close WS before exit     | SATISFIED   | index.ts: shutdown() → terminateAll() → unregister() → process.exit; 5s drain       |

All 13 Phase 2 requirement IDs from REQUIREMENTS.md are accounted for and satisfied in code.

**Orphaned requirements check:** REQUIREMENTS.md traceability table maps SIP-03, SIP-04, SIP-05, WSB-01–07, CON-01, CON-02, LCY-01 to Phase 2. All 13 are covered by plans and verified above. No orphaned requirements.

---

### Anti-Patterns Found

| File                         | Line | Pattern                                    | Severity | Impact                                               |
|------------------------------|------|--------------------------------------------|----------|------------------------------------------------------|
| `src/bridge/callManager.ts`  | 240  | callLog created before streamSid generated | INFO     | Per-call logger lacks streamSid binding (OBS-01 gap, Phase 1 requirement, not Phase 2) |

No blockers or warnings. The single info-level item (`callLog` created at line 240 without `streamSid`, which is generated at line 250) means the per-call pino logger omits the `streamSid` binding on log events emitted during RTP allocation. This was noted by the planner as acceptable ("callLog created before RTP allocation"). OBS-01 is a Phase 1 requirement and was marked complete there; this is a minor gap in binding completeness, not a Phase 2 failure.

---

### Human Verification Required

#### 1. Connected + Start Event Ordering

**Test:** Use a real sipgate SIP trunk to make a test call to the service; attach a WebSocket listener at WS_TARGET_URL.
**Expected:** The WS listener receives `connected` first, then `start` (with `streamSid`, `callSid`, `customParameters.From`, `customParameters.To`, `customParameters.sipCallId`) — all before the first `media` event arrives.
**Why human:** Event ordering under real UDP/SIP jitter and WebSocket handshake timing cannot be asserted programmatically without a live integration harness.

#### 2. Inbound Audio (RTP → WS media events)

**Test:** During a live test call, observe `media` events at the WS listener while speaking into the phone.
**Expected:** Each `media` event contains a base64 PCMU payload; decoding and playing back the audio yields intelligible speech at 8kHz.
**Why human:** Requires a live RTP stream; correct header-stripping and base64 encoding cannot be verified without actual UDP packets from a real SIP device.

#### 3. Outbound Audio (WS media events → RTP playback)

**Test:** From the WS backend, inject base64 PCMU audio as `media` events during an active call; verify the caller hears the audio.
**Expected:** The injected audio plays audibly to the caller with no distortion.
**Why human:** Requires audible verification with a real caller; correct RTP header construction and UDP delivery are runtime-only concerns.

#### 4. SIP 503 on Unreachable WS Backend

**Test:** Start the service with WS_TARGET_URL pointing to a closed port; place an inbound test call.
**Expected:** The caller receives a SIP 503 response within approximately 2 seconds (the WS connect timeout window).
**Why human:** Requires a live SIP call and a deliberately closed WS port; 2-second timeout is runtime-enforced.

#### 5. Two Concurrent Calls — Isolation

**Test:** Simultaneously originate two inbound SIP calls; attach two separate WS listeners; cross-check `streamSid` values.
**Expected:** Each listener receives a unique `streamSid`; audio spoken on line A does not appear on line B's WS connection.
**Why human:** Requires two concurrent SIP sessions; stream isolation under real concurrency cannot be verified statically.

#### 6. SIGTERM Graceful Shutdown

**Test:** While a live call is active, send SIGTERM (or `docker stop`) to the service.
**Expected:** Caller receives SIP BYE; WS backend receives a `stop` event; the service logs `shutdown_complete` and exits cleanly (exit code 0) within 5 seconds.
**Why human:** Signal delivery, drain timing, and actual BYE/stop sequencing require a running process with live connections.

---

### Gaps Summary

No gaps. All 13 Phase 2 requirements (SIP-03/04/05, WSB-01–07, CON-01/02, LCY-01) have substantive, wired implementations. TypeScript compiles cleanly. The 6 human verification items above reflect the inherent need for live-call integration testing to confirm runtime correctness of the full audio bridge; they are not code deficiencies.

---

_Verified: 2026-03-03_
_Verifier: Claude (gsd-verifier)_
