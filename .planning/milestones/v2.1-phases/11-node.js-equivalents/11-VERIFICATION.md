---
phase: 11-node.js-equivalents
verified: 2026-03-05T17:10:00Z
status: passed
score: 12/12 must-haves verified
re_verification: false
---

# Phase 11: Node.js Equivalents Verification Report

**Phase Goal:** The Node.js reference implementation has full parity with Go for mark/clear events and SIP OPTIONS keepalive
**Verified:** 2026-03-05T17:10:00Z
**Status:** passed
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | Node.js echoes mark name after all preceding audio frames drain; echoes immediately if queue idle | VERIFIED | `makeDrain` tagged-union + `enqueueMark` fast-path on line 382-394 wsClient.ts; test `outboundDrain mark sentinel — MRKN-01` passes (2 cases) |
| 2 | Node.js flushes audio queue on clear event and echoes all pending mark names immediately | VERIFIED | `stop()` iterates queue echoing non-Buffer items before `queue.length = 0` (lines 396-408 wsClient.ts); `sendClear()` delegates to `outboundDrain.stop()` (line 295); test `outboundDrain sendClear — MRKN-02` passes |
| 3 | WsClient interface exposes `onMark`, `sendMark`, `sendClear` — call managers use the interface | VERIFIED | Interface declaration lines 23-28 wsClient.ts; callManager uses `ws.onMark` (line 495) and `session.ws.sendMark` (line 496) via interface, not raw socket |
| 4 | Node.js sends SIP OPTIONS every 30s, triggers re-registration on timeout/error (not on 401/407) | VERIFIED | `setInterval` at line 249 with `config.SIP_OPTIONS_INTERVAL * 1000`; `applyOptionsResponse` exported; 8 unit tests all green |
| 5 | OPTIONS keepalive interval stopped cleanly when User-Agent shuts down | VERIFIED | `SipHandle.stop()` clears `refreshTimer`, `optionsTimer`, `pingTimer` and closes socket (lines 267-270 userAgent.ts) |

**Score:** 5/5 truths verified

---

## Required Artifacts

### Plan 11-01 Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `node/test/wsClient.mark.test.ts` | Failing test stubs for mark/clear behavior | VERIFIED | 4 real `expect()` tests (not todos), all green |
| `node/test/userAgent.options.test.ts` | Failing test stubs for OPTIONS response classification | VERIFIED | 8 real `expect()` tests, all green |
| `node/package.json` | Vitest installed + test script `"test": "vitest run"` | VERIFIED | `vitest@^4.0.18` in devDependencies; `"test": "vitest run"` in scripts |

### Plan 11-02 Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `node/src/ws/wsClient.ts` | Extended WsClient interface + tagged-union drain + mark/clear dispatch; exports `WsClient`, `createWsClient`; contains `onMark` | VERIFIED | Interface at lines 12-29; `DrainItem` type line 32; `onMark`/`sendMark`/`sendClear` implemented lines 272-296; message dispatch `case 'mark'` and `case 'clear'` lines 248-257 |
| `node/src/bridge/callManager.ts` | ws.onMark wiring in handleInvite and startWsReconnectLoop | VERIFIED | `ws.onMark(...)` at line 495 (handleInvite); `newWs.onMark(...)` at line 697 (startWsReconnectLoop) |
| `node/test/wsClient.mark.test.ts` | Green unit tests for MRKN-01/02/03 | VERIFIED | All 4 tests pass: 2 for MRKN-01, 1 for MRKN-02, 1 for MRKN-03 |

### Plan 11-03 Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `node/src/config/index.ts` | `SIP_OPTIONS_INTERVAL` field in Zod schema | VERIFIED | Line 17: `SIP_OPTIONS_INTERVAL: z.coerce.number().int().positive().default(30)` |
| `node/src/sip/userAgent.ts` | OPTIONS keepalive setInterval + buildOptions + `applyOptionsResponse` export + `optionsTimer` | VERIFIED | `applyOptionsResponse` exported lines 100-115; `buildOptions` lines 162-178; `optionsTimer` declared line 136; `setInterval` starts at line 249; `handleOptionsResponse` lines 180-193 |
| `node/test/userAgent.options.test.ts` | Green unit tests for OPTN-01/02/03 | VERIFIED | All 8 tests pass covering 200 OK, 1st/2nd failure, timeout, 401, 407, 404, 500 |

---

## Key Link Verification

### Plan 11-02 Key Links

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `node/src/ws/wsClient.ts` | `makeDrain onMarkReady callback` | closure-captured `markHandler` variable set by `onMark()` | VERIFIED | Line 158: `(name) => markHandler?.(name)` — optional-chaining call; `markHandler` set at line 273 via `onMark()` |
| `node/src/bridge/callManager.ts` | `ws.onMark` | called after ws.onAudio in handleInvite AND startWsReconnectLoop | VERIFIED | Line 495: `ws.onMark(...)` follows `ws.onAudio(...)` at line 485; line 697: `newWs.onMark(...)` follows `newWs.onAudio(...)` at line 691 |

### Plan 11-03 Key Links

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `node/src/sip/userAgent.ts` | `socket.on('message') handler` | CSeq header routing: OPTIONS responses routed to handleOptionsResponse | VERIFIED | Lines 211-218: `cseqVal.includes('OPTIONS')` guard routes to `handleOptionsResponse` and returns before REGISTER branch |
| `node/src/sip/userAgent.ts` | `SipHandle.stop()` | `clearInterval(optionsTimer)` + `clearTimeout(pingTimer)` | VERIFIED | Lines 267-270: `clearInterval(optionsTimer)` and `clearTimeout(pingTimer)` both present in `stop()` body |

---

## Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|-------------|-------------|--------|----------|
| MRKN-01 | 11-02 | Node.js echoes mark name after all preceding audio frames playout; echoes immediately on empty queue | SATISFIED | Tagged-union drain with `enqueueMark` fast-path; 2 green tests |
| MRKN-02 | 11-02 | Node.js clear event flushes audio queue and echoes pending marks immediately | SATISFIED | `stop()` echoes non-Buffer items before zeroing queue; `sendClear()` delegates; 1 green test |
| MRKN-03 | 11-02 | WsClient interface extended with `onMark`, `sendMark`, `sendClear` | SATISFIED | Interface declaration verified; `sendMark` JSON format verified by MRKN-03 test |
| OPTN-01 | 11-03 | Node.js sends SIP OPTIONS every 30s (configurable) | SATISFIED | `setInterval(..., config.SIP_OPTIONS_INTERVAL * 1000)` in userAgent.ts; default 30 in config |
| OPTN-02 | 11-03 | Timeout/error triggers re-registration; 401/407 does not | SATISFIED | `applyOptionsResponse` pure function; 8 tests covering all response classes; CSeq routing prevents OPTIONS 401 from corrupting REGISTER state machine |
| OPTN-03 | 11-03 | OPTIONS keepalive stops cleanly on User-Agent shutdown | SATISFIED | `stop()` clears `optionsTimer` (clearInterval) and `pingTimer` (clearTimeout) |

**Orphaned requirements check:** No additional Phase 11 requirements in REQUIREMENTS.md beyond MRKN-01/02/03 and OPTN-01/02/03. All 6 mapped requirements are accounted for.

---

## Test Suite Results

```
vitest run v4.0.18

PASS  test/userAgent.options.test.ts  (8 tests)
PASS  test/wsClient.mark.test.ts      (4 tests)

Test Files:  2 passed (2)
Tests:      12 passed (12)
Duration:   165ms
```

`pnpm exec tsc --noEmit` exits 0 — no TypeScript errors.

---

## Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| — | — | None | — | — |

No TODOs, FIXMEs, placeholder returns, `it.todo` stubs, or empty implementations found in any Phase 11 modified file.

---

## Human Verification Required

### 1. End-to-End mark/clear echo with live sipgate trunk

**Test:** Connect a Twilio Media Streams client, send a `mark` event mid-TTS-playback, confirm the echo arrives after audio drains.
**Expected:** The WebSocket backend receives a `{ event: "mark", mark: { name: "..." } }` JSON message exactly after the preceding audio frames finish playing at the caller's end.
**Why human:** Requires a live sipgate PSTN call and a real TTS backend; timer-based audio pacing cannot be exercised in unit tests without a real UDP RTP stream.

### 2. OPTIONS keepalive triggers re-register under real network loss

**Test:** Start the Node.js agent against sipgate, block UDP port 5060 outbound for 65+ seconds (2 consecutive 30s intervals with 5s timeout each), then unblock.
**Expected:** Log shows `options_reregister` after the 2nd failure, registration is restored, and subsequent calls proceed normally.
**Why human:** Requires actual network manipulation; the interval timer and DNS resolution involve real system calls that cannot be reliably emulated in unit tests.

---

## Gaps Summary

None. All 12 automated unit tests pass, all 6 requirements are satisfied, all key wiring links are present in the codebase, and TypeScript compiles cleanly. The phase goal — Node.js parity with Go for mark/clear events and SIP OPTIONS keepalive — is fully achieved.

---

_Verified: 2026-03-05T17:10:00Z_
_Verifier: Claude (gsd-verifier)_
