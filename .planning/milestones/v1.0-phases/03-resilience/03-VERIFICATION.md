---
phase: 03-resilience
verified: 2026-03-03T22:00:00Z
status: passed
score: 9/9 must-haves verified
re_verification: false
---

# Phase 3: Resilience Verification Report

**Phase Goal:** The service survives WebSocket drops mid-call without hanging up the caller, and correctly releases all file descriptors after calls end
**Verified:** 2026-03-03T22:00:00Z
**Status:** PASSED
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| 1 | When WS disconnects mid-call, the SIP call stays up and the caller hears RTP silence (not dead air) | VERIFIED | `ws.onDisconnect` sets `session.wsReconnecting = true` and calls `startWsReconnectLoop` instead of `terminateSession`. Silence interval fires `session.rtp.sendAudio(Buffer.alloc(160, 0xff))` every 20ms throughout the reconnect window (callManager.ts:480-485, 625-627) |
| 2 | Service reconnects to WS with 1s/2s/4s/4s... backoff, up to 30 seconds total | VERIFIED | `BUDGET_MS=30_000`, `CAP_MS=4_000`, initial `delay=1_000`, doubled via `Math.min(delay*2, CAP_MS)` on each failed attempt (callManager.ts:619-622, 671) |
| 3 | After reconnect, WS backend receives a fresh connected then start event before any media events resume | VERIFIED | `createWsClient` is called with original `wsParams` (same streamSid/callSid) on reconnect. `wsClient.ts` unconditionally sends `connected` then `start` in the `ws.once('open')` handler before resolving the promise (wsClient.ts:44-79). `session.wsReconnecting` is set to false only AFTER `createWsClient` resolves (callManager.ts:644) |
| 4 | Inbound RTP from caller is dropped (not forwarded) while wsReconnecting is true | VERIFIED | First line of `rtp.on('audio')` handler: `if (session.wsReconnecting) return;` — early return drops the packet (callManager.ts:453) |
| 5 | If the 30s budget is exhausted, terminateSession is called with sendBye=true | VERIFIED | Budget check `elapsed + delay >= BUDGET_MS` calls `this.terminateSession(session, 'ws_reconnect_failed', true)` (callManager.ts:663-667) |
| 6 | If BYE arrives during reconnect, the reconnect loop exits without zombie reconnects | VERIFIED | At the top of each `attempt(n)` call: `if (!this.sessions.has(session.callId)) { cleanup(); return; }` — BYE handler calls `terminateSession` which deletes from `this.sessions`, causing the guard to exit (callManager.ts:632-636) |
| 7 | After 20 sequential calls, the process FD count returns to baseline (no FD leak) | VERIFIED | `node --import tsx/esm test/fd-leak.mjs` exits 0 with delta=0: "Baseline FDs: 46 / Final FDs: 46 / Delta: 0 / PASS: no FD leak detected" |
| 8 | test/fd-leak.mjs works on both macOS (lsof) and Linux (/proc/self/fd) | VERIFIED | `getFdCount()` tries `/proc/self/fd` first, catches error and falls back to `lsof -p $PID`. Both paths implemented and the script ran cleanly on macOS (test/fd-leak.mjs:17-27) |
| 9 | TypeScript compiles cleanly | VERIFIED | `pnpm exec tsc --noEmit` produces zero errors |

**Score:** 9/9 truths verified

---

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `src/bridge/callManager.ts` | `wsReconnecting: boolean` field on `CallSession` interface | VERIFIED | Line 57: `wsReconnecting: boolean` with JSDoc comment referencing WSR-03 |
| `src/bridge/callManager.ts` | `wsReconnecting: false` in session literal | VERIFIED | Line 432: `wsReconnecting: false` in the `CallSession` object constructed in `handleInvite` |
| `src/bridge/callManager.ts` | `startWsReconnectLoop` private method | VERIFIED | Lines 615-677: full implementation with silence interval, backoff loop, BYE-race guard, session.ws swap on success |
| `src/bridge/callManager.ts` | `wsReconnecting` gate in `rtp.on('audio')` handler | VERIFIED | Line 453: `if (session.wsReconnecting) return;` as first statement in handler |
| `src/bridge/callManager.ts` | DTMF and audio routed via `session.ws` (not closure-captured `ws`) | VERIFIED | Line 458: `session.ws.sendAudio(payload)`, line 461: `session.ws.sendDtmf(digit)` |
| `test/fd-leak.mjs` | Standalone ESM FD leak test: 20x createRtpHandler + dispose() | VERIFIED | 78-line ESM script — `getFdCount`, `noopLog`, 20-iteration loop, 50ms delay, delta check with ±2 tolerance, correct exit codes |

---

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `callManager.ts` `ws.onDisconnect` handler | `startWsReconnectLoop` | Replaces old `terminateSession` call | WIRED | Lines 480-485: `session.wsReconnecting = true; void this.startWsReconnectLoop(session, wsParams)`. Old `terminateSession.*ws_disconnect` pattern absent (grep returned no matches) |
| `startWsReconnectLoop` | `createWsClient` | Re-called with same `WsCallParams` (streamSid, callSid, from, to, sipCallId) | WIRED | Line 641: `createWsClient(this.config.WS_TARGET_URL, params, session.log)` — `params` carries original identifiers captured at line 473 |
| `createWsClient` on reconnect | WS backend `connected` + `start` events | Auto-sent in `ws.once('open')` before promise resolves | WIRED | `wsClient.ts` lines 44-79 unconditionally send both events on open. Re-calling `createWsClient` with same params satisfies WSR-02 by construction |
| `rtp.on('audio')` | `session.wsReconnecting` | Early-return gate drops packets during reconnect | WIRED | Line 453: gate is first statement — no audio reaches `session.ws.sendAudio` while flag is true |
| `test/fd-leak.mjs` | `src/rtp/rtpHandler.ts` | `createRtpHandler` + `dispose()` in a 20-iteration loop | WIRED | Line 51: `createRtpHandler({ portMin, portMax, log: noopLog })`, line 52: `rtp.dispose()` — script runs and passes |
| `startWsReconnectLoop` success branch | recursive `newWs.onDisconnect` | Recursive re-wiring so future drops also handled | WIRED | Lines 653-658: `newWs.onDisconnect(() => { session.wsReconnecting = true; void this.startWsReconnectLoop(session, params)... })` |

---

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|---------|
| WSR-01 | 03-01-PLAN.md | If WebSocket disconnects during an active call, service reconnects with exponential backoff | SATISFIED | `startWsReconnectLoop`: 30s budget, 1s/2s/4s cap backoff, silence injection, BYE-race guard — all implemented and wired |
| WSR-02 | 03-01-PLAN.md | After WebSocket reconnect, service re-sends `connected` then `start` before forwarding audio | SATISFIED | `createWsClient` auto-sends both events on `ws.open`; reconnect loop calls it with original `wsParams`; `wsReconnecting` flag keeps RTP gated until `createWsClient` resolves |
| WSR-03 | 03-01-PLAN.md + 03-02-PLAN.md | Audio arriving from RTP during WebSocket reconnect window is dropped (not buffered indefinitely) | SATISFIED | `rtp.on('audio')` early-return gate (03-01); `dispose()` closes dgram socket releasing FDs; FD leak test proves socket cleanup (03-02) |

**Orphaned requirements check:** REQUIREMENTS.md traceability table maps WSR-01, WSR-02, WSR-03 exclusively to Phase 3. No Phase 3 requirements are orphaned. Note: `CON-02` ("no file descriptor leak") is mapped to Phase 2 in the traceability table but Phase 3 Plan 02 explicitly verifies it — this is additive, not a conflict.

---

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| — | — | — | — | None found |

Scanned `src/bridge/callManager.ts` and `test/fd-leak.mjs` for: TODO/FIXME/XXX/HACK/PLACEHOLDER, empty implementations (`return null`, `return {}`, `return []`), console.log-only handlers, and stub wiring. No issues detected.

---

### Human Verification Required

#### 1. Live WS drop mid-call audio continuity

**Test:** Place a real SIP call to the service. While audio is flowing, abruptly kill the WebSocket backend process. Wait 2-3 seconds, then restart the backend.
**Expected:** The SIP call remains up throughout. The caller hears ~20ms silence packets during the reconnect window (not dead air / silence > 1 second). After the backend restarts, audio resumes in both directions.
**Why human:** Cannot verify μ-law silence audibility or RTP continuity programmatically without a live SIP endpoint.

#### 2. Budget-exhaustion BYE path

**Test:** Place a real SIP call, kill the WS backend, and keep it down for 35+ seconds.
**Expected:** After 30 seconds, the service sends SIP BYE to the caller and the call ends cleanly on both sides.
**Why human:** Requires live SIP stack; the 30-second timer cannot be practically triggered in unit-test scope without a harness.

---

### Gaps Summary

No gaps. All automated checks pass:
- TypeScript compiles with zero errors
- `test/fd-leak.mjs` exits 0 with delta=0 on macOS
- `wsReconnecting` field, `startWsReconnectLoop` method, silence interval, BYE-race guard, session.ws routing, recursive onDisconnect re-wiring — all present and correctly wired
- Old `terminateSession('ws_disconnect')` handler completely replaced (grep confirms absence)
- WSR-01, WSR-02, WSR-03 all satisfied with implementation evidence

Phase 3 goal is achieved. Two human verification items are flagged for completeness but do not block the overall PASSED verdict.

---

_Verified: 2026-03-03T22:00:00Z_
_Verifier: Claude (gsd-verifier)_
