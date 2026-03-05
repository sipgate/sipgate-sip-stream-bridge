---
phase: 06-inbound-call-rtp-bridge
plan: 03
status: completed
completed: 2026-03-04
---

# 06-03 Summary: WebSocket Bridge + Runtime Bug Fixes

## What Was Built

Completed the full Twilio Media Streams WebSocket bridge (ws.go) and wired Phase 6 into
main.go, then applied 4 runtime bugs discovered during live testing.

### ws.go — Full Twilio Media Streams Implementation
- `dialWS` — gobwas/ws dial (ws:// and wss:// via system roots)
- `sendConnected` — WSB-01: `{event:connected,protocol:Call,version:1.0.0}`
- `sendStart` — WSB-02+WSB-06: callSidToken (CA-prefixed), sipCallId in customParameters
- `sendStop` — WSB-04: CA-prefixed callSid in stop body
- `writeJSON` — wsutil.WriteClientText (RFC 6455 client masking)
- `readWSMessage` — wsutil.ReadServerData wrapper (consistent test/prod path)

### main.go — Phase 6 Wiring
PortPool + CallManager + sip.NewHandler inserted before registrar.Register().

## Runtime Bug Fixes (4 bugs from live testing)

**Bug 1 — User-Agent** (`internal/sip/agent.go`):
- `sipgo.WithUserAgentName` → `sipgo.WithUserAgent` (correct API in sipgo v1.2.0)

**Bug 2 — SID Format** (`internal/bridge/manager.go`, `ws.go`):
- `streamSid`: `MZ` + 32 hex chars (no dashes) — was `MZ` + raw UUID with dashes
- `callSidToken`: new `CA` + 32 hex field, distinct from SIP Call-ID
- `sendStart` now uses `callSidToken` for `callSid`; SIP Call-ID moved to
  `customParameters.sipCallId` so consumers can correlate to SIP layer

**Bug 3 — Ring Before Answer** (`internal/sip/handler.go`, `internal/bridge/manager.go`,
`internal/bridge/session.go`):
- `onInvite`: added 180 Ringing after 100 Trying; removed SDP build + RespondSDP
- `StartSession`: now dials WS first (10 s timeout from dlg.Context()); rejects with 503
  if WS unreachable; builds SDP answer; sends 200 OK; then enters `session.run()`
- `session.run()`: accepts pre-dialed `wsConn net.Conn` — no more dialWS inside run()

**Bug 4 — rtpToWS Jitter** (`internal/bridge/session.go`):
- Removed 100 ms `SetReadDeadline` poll loop from `rtpToWS` — this caused burst packets
  when goroutine was briefly descheduled (deadline fired → all buffered UDP read at once)
- Replaced with plain blocking `ReadFromUDP`; `ctx.Err()` check on error handles clean exit
- `run()` now calls `rtpConn.Close()` before `wg.Wait()` to unblock the blocking read

## Key Decisions Made

- `sipgo.WithUserAgent(ua string)` is the correct v1.2.0 API (not `WithUserAgentName`)
- WS dial belongs in StartSession, not session.run() — must precede 200 OK
- `callSidToken` (CA-prefixed) is the Twilio callSid; SIP Call-ID ≠ Twilio callSid
- `rtpConn.Close()` + blocking read is cleaner than 100 ms deadline polling for shutdown

## Tests

- `TestSendStart_JSONSchema`: verifies callSidToken as callSid, sipCallId in customParameters
- `TestSendStop_JSONSchema`: verifies CA-prefixed token in stop body
- `TestSendConnected_JSONSchema`, `TestWriteJSON_ReadWSMessage_RoundTrip`: unchanged
- All tests pass with `-race`

## Files Modified

- `internal/sip/agent.go` — WithUserAgent fix
- `internal/sip/handler.go` — 180 Ringing + removed SDP/200OK from onInvite
- `internal/bridge/manager.go` — SID generation, WS dial, SDP answer, 200 OK in StartSession
- `internal/bridge/ws.go` — sendStart/sendStop signature updates
- `internal/bridge/ws_test.go` — updated for new sendStart signature
- `internal/bridge/session.go` — run() signature, rtpToWS blocking read, shutdown sequence
- `cmd/sipgate-sip-stream-bridge/main.go` — Phase 6 wiring (PortPool + CallManager + Handler)
