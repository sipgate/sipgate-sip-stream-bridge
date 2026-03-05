---
phase: 07-websocket-resilience-dtmf
verified: 2026-03-04T00:00:00Z
status: passed
score: 9/9 must-haves verified
---

# Phase 7: WebSocket Resilience + DTMF Verification Report

**Phase Goal:** When the WebSocket drops mid-call, the service reconnects with exponential backoff, re-sends the handshake sequence, and resumes audio forwarding — DTMF digits pressed by the caller are forwarded as `dtmf` events exactly once per keypress
**Verified:** 2026-03-04
**Status:** passed
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| #  | Truth                                                                                                                       | Status     | Evidence                                                                                                                                                                                                        |
|----|-----------------------------------------------------------------------------------------------------------------------------|------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 1  | A WS error during an active call does NOT immediately send SIP BYE                                                         | VERIFIED   | `wsToRTP` calls `sig.Signal()` (not `dlg.Bye()`) on read error; `wsPacer` calls `sig.Signal()` (not `s.cancel()`) on write error; only budget exhaustion or "stop" event triggers BYE                          |
| 2  | Service retries WS connection with 1s/2s/4s backoff, stopping after 30s total budget                                       | VERIFIED   | `reconnect()` in session.go:198-229 — `budget := context.WithTimeout(ctx, 30s)`, `backoff := time.Second`, `maxBackoff = 4s`, doubles each attempt; context-aware select guards each sleep                     |
| 3  | After successful reconnect, consumer receives `connected` then `start` before any audio                                    | VERIFIED   | `handshake()` called at session.go:107 (initial) and 169 (reconnect path); handshake sends `sendConnected` then `sendStart` in order; WS goroutines relaunched only after `handshake()` returns nil             |
| 4  | RTP packets arriving during the reconnect window are dropped silently; rtpReader is never blocked                          | VERIFIED   | `rtpReader` uses non-blocking enqueue to `rtpInboundQueue` with `select default` drop; `rtpReader` runs persistently — never stopped during reconnect; bounded queue absorbs bursts then drops                  |
| 5  | Budget exhaustion triggers SIP BYE and clean session shutdown                                                              | VERIFIED   | session.go:159-165 — `reconnect()` returns `(nil, false)` → `dlg.Bye()` + `s.cancel()` + `rtpConn.Close()` + `s.wg.Wait()` + `return`                                                                         |
| 6  | Pressing a DTMF digit produces exactly one `dtmf` event (RFC 4733 End-bit dedup by RTP timestamp)                         | VERIFIED   | `rtpReader` checks `pkt.PayloadType == s.dtmfPT`, calls `parseTelephoneEvent`, guards on `pkt.Header.Timestamp == s.lastDtmfTS`; dedup test `TestDTMFDeduplication_SameTimestamp` confirms 3x same-ts = 1 entry |
| 7  | Holding a key (End=0 packets) produces zero `dtmf` events until key is released                                            | VERIFIED   | session.go:262 — `if !ok \|\| !isEnd { continue }` drops all non-End packets silently before dedup check                                                                                                        |
| 8  | Three End=1 retransmissions with same timestamp produce exactly one `dtmf` event                                           | VERIFIED   | `TestDTMFDeduplication_SameTimestamp` passes: 3 identical End packets with `ts=12345` → 1 queue entry; `TestDTMFForwarding_NewTimestamp` confirms distinct timestamps each produce one entry                    |
| 9  | DTMF events forwarded immediately (not held for 20ms audio tick); rtpReader never blocked on DTMF                          | VERIFIED   | `wsPacer` select: `case digit := <-s.dtmfQueue` appears before `case <-ticker.C` (session.go:325); `dtmfQueue` send in `rtpReader` is non-blocking (select/default at session.go:273-277)                      |

**Score:** 9/9 truths verified

---

### Required Artifacts

| Artifact                          | Expected                                                                              | Status     | Details                                                                                                              |
|-----------------------------------|---------------------------------------------------------------------------------------|------------|----------------------------------------------------------------------------------------------------------------------|
| `internal/bridge/session.go`      | Refactored `run()` with reconnect loop; `wsSignal`; `reconnect()`; `handshake()`     | VERIFIED   | All four elements present and substantive; 532 lines; reconnect loop at lines 122-179; methods at 187-229           |
| `internal/bridge/ws.go`           | `wsSignal` type with `sync.Once` guard; `DtmfEvent`; `DtmfBody`; `sendDTMF`; `parseTelephoneEvent` | VERIFIED   | All types and functions present and substantive (244 lines); `wsSignal` at 206-222; DTMF types at 175-200; parser at 161-171 |
| `internal/bridge/ws_test.go`      | Tests for wsSignal, handshake, parseTelephoneEvent, sendDTMF, dedup                 | VERIFIED   | 544 lines; 19 tests total; all 19 pass with -race flag; covers every specified test name                            |

---

### Key Link Verification

| From                                  | To                                          | Via                                                    | Status  | Details                                                                                                        |
|---------------------------------------|---------------------------------------------|--------------------------------------------------------|---------|----------------------------------------------------------------------------------------------------------------|
| `session.go run()`                    | `session.go reconnect()`                    | `case <-sig.Done()` channel signal                     | WIRED   | session.go:147 — `case <-sig.Done():` triggers reconnect path; `s.reconnect(sessionCtx)` called at line 158   |
| `session.go reconnect()`              | `ws.go dialWS`                              | Exponential backoff loop                               | WIRED   | session.go:215 — `conn, err := dialWS(dialCtx, s.cfg.WSTargetURL)` with `budget` and `dialCtx`                |
| `session.go run()`                    | `session.go handshake()`                    | Called after each successful reconnect before WS goroutines restart | WIRED   | session.go:107 (initial), 169 (reconnect): both paths call `s.handshake()` before WS goroutines launch         |
| `session.go rtpReader`                | `session.go CallSession.dtmfQueue`          | Non-blocking select send when `isEnd && timestamp != lastDtmfTS` | WIRED   | session.go:274 — `case s.dtmfQueue <- digit:` with `default` drop at line 275                                 |
| `session.go wsPacer`                  | `ws.go sendDTMF`                            | `case digit := <-s.dtmfQueue` in wsPacer select        | WIRED   | session.go:325 — `case digit := <-s.dtmfQueue:` calls `sendDTMF(wsConn, s.streamSid, digit, seqNo)` at 328   |
| `ws.go parseTelephoneEvent`           | `session.go rtpReader`                      | Called with `pkt.Payload` when `pkt.PayloadType == s.dtmfPT` | WIRED   | session.go:261 — `digit, isEnd, ok := parseTelephoneEvent(pkt.Payload)`                                       |

---

### Requirements Coverage

| Requirement | Source Plan | Description                                                                                          | Status    | Evidence                                                                                                        |
|-------------|-------------|------------------------------------------------------------------------------------------------------|-----------|-----------------------------------------------------------------------------------------------------------------|
| WSR-01      | 07-01-PLAN  | WS reconnects with 1s/2s/4s exponential backoff, 30s budget; call not torn down during attempts     | SATISFIED | `reconnect()` at session.go:198-229 with `context.WithTimeout(ctx, 30s)`, backoff 1s→2s→4s; `run()` reconnect loop does not call `dlg.Bye()` until budget exhausted |
| WSR-02      | 07-01-PLAN  | After reconnect, re-sends `connected` then `start` before forwarding audio                           | SATISFIED | `handshake()` called at session.go:169 after every successful dial; WS goroutines not launched until handshake returns nil; `handshake()` always sends connected+start in order |
| WSR-03      | 07-01-PLAN  | Audio arriving during reconnect window is dropped; service does not accumulate memory or block RTP   | SATISFIED | `rtpInboundQueue` is bounded (50 slots); `rtpReader` non-blocking enqueue with drop+warn; `rtpReader` runs continuously during reconnect (never stopped) |
| WSB-07      | 07-02-PLAN  | DTMF digits forwarded as `dtmf` events; RFC 4733 End-bit dedup by RTP timestamp; exactly once per keypress | SATISFIED | Full pipeline: `parseTelephoneEvent` + `lastDtmfTS` dedup + `dtmfQueue` enqueue in `rtpReader`; `sendDTMF` called from `wsPacer`; 19 bridge tests all pass |

No orphaned requirements — all 4 IDs (WSR-01, WSR-02, WSR-03, WSB-07) appear in both plan frontmatter and REQUIREMENTS.md traceability table, all marked Complete for Phase 7.

---

### Anti-Patterns Found

No anti-patterns found in modified files.

Scan performed on `internal/bridge/session.go` and `internal/bridge/ws.go`:
- No TODO/FIXME/HACK/PLACEHOLDER comments
- No stub return values (`return nil` in `reconnect()` at line 210 is a legitimate sentinel return when budget is exhausted — it is documented, deliberate, and tested)
- No empty implementations
- No console.log-only implementations

---

### Human Verification Required

The following items cannot be verified programmatically and require an active SIP call with a real or simulated WebSocket drop:

**1. Reconnect survives a real mid-call WS drop**

Test: Establish an active call, kill the WS server mid-call, restart it within 30 seconds.
Expected: Service logs reconnect attempts with backoff intervals (~1s, ~2s, ~4s); after the WS server restarts, audio resumes without the caller hearing a SIP BYE; the WS consumer receives a fresh `connected` + `start` event pair.
Why human: The reconnect loop timing and real WS server failure cannot be exercised by unit tests.

**2. Budget exhaustion sends BYE after 30s**

Test: Establish a call, kill the WS server and keep it down for more than 30 seconds.
Expected: After ~30 seconds the service sends SIP BYE to the caller, call is torn down cleanly, no goroutine/FD leak.
Why human: Requires wall-clock 30s wait and external SIP verification.

**3. DTMF digit appears exactly once on WS for a single keypress**

Test: During an active call, press a single digit on the caller's phone.
Expected: Exactly one `dtmf` JSON event appears on the WS with the correct digit; no duplicate within the same keypress.
Why human: Requires real RTP telephone-event packets from a SIP phone, which unit tests cannot produce end-to-end.

---

### Build and Test Verification

```
go build ./...           PASS  (zero compilation errors)
go test ./internal/bridge/... -race
                         PASS  19/19 tests, no data races
```

Test names covering phase 7 functionality:
- `TestWsSignal_MultipleSignalsNoPanic` — sync.Once guard, no double-close panic
- `TestWsSignal_DoneClosedAfterSignal` — Done() channel semantics
- `TestHandshake_SendsConnectedThenStart` — connected+start order on reconnect
- `TestParseTelephoneEvent_ShortPayload` — RFC 4733 parser validation
- `TestParseTelephoneEvent_EventCodeTooHigh` — RFC 4733 parser bounds check
- `TestParseTelephoneEvent_Digits` — digit mapping codes 0-15
- `TestParseTelephoneEvent_EndBit` — End-bit detection
- `TestSendDTMF_JSONSchema` — Twilio dtmf JSON schema compliance
- `TestDTMFDeduplication_SameTimestamp` — RFC 4733 3x retransmit produces exactly 1 entry
- `TestDTMFForwarding_NewTimestamp` — distinct timestamps produce distinct entries

---

### Structural Invariants Verified

- `s.wg` tracks only `rtpReader` + `rtpPacer` (2 goroutines) — confirmed at session.go:116 (`s.wg.Add(2)`)
- `wsPacer` and `wsToRTP` use per-connection `wsWg` — confirmed at session.go:124-125 (`wsWg.Add(2)`)
- `rtpConn.Close()` is NOT called during a normal reconnect loop iteration — called only on shutdown paths (lines 98 defer, 141 sessionCtx.Done, 163 budget exhausted)
- `defer wsConn.Close()` removed from `run()`; each return path closes explicitly (wsConn is reassigned in loop)
- `dtmfQueue` initialized at session.go:103 BEFORE `s.wg.Add(2)` at line 116 — no nil-channel race

---

_Verified: 2026-03-04_
_Verifier: Claude (gsd-verifier)_
