---
phase: 07-websocket-resilience-dtmf
plan: 02
subsystem: bridge
tags: [rtp, dtmf, rfc4733, websocket, twilio]

# Dependency graph
requires:
  - phase: 07-01
    provides: wsSignal, wsPacer/wsToRTP per-connection goroutine model, reconnect loop
  - phase: 06-inbound-call-rtp-bridge
    provides: rtpReader, wsPacer, CallSession, rtpInboundQueue, dtmfPT field
provides:
  - parseTelephoneEvent: RFC 4733 4-byte telephone-event payload parser
  - DtmfEvent + DtmfBody: Twilio Media Streams dtmf JSON schema structs
  - sendDTMF: WS writer for dtmf events (sole wsPacer writer invariant maintained)
  - dtmfQueue channel: non-blocking rtpReader-to-wsPacer digit delivery
  - lastDtmfTS: RFC 4733 End-packet retransmission deduplication
  - Full DTMF pipeline: rtpReader parse+dedup+enqueue, wsPacer drain+forward
affects: [phase-08-production-hardening]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "RFC 4733 dedup by RTP timestamp: End=1 packets with duplicate timestamps are dropped"
    - "Non-blocking dtmfQueue send with warn log on full queue — rtpReader never blocks on DTMF"
    - "dtmfQueue case before ticker.C in wsPacer select — DTMF given priority over audio ticks"
    - "TDD: RED (failing tests) then GREEN (implementation) for both parseTelephoneEvent and sendDTMF"

key-files:
  created: []
  modified:
    - internal/bridge/ws.go
    - internal/bridge/ws_test.go
    - internal/bridge/session.go

key-decisions:
  - "parseTelephoneEvent and sendDTMF placed in ws.go alongside other WS event helpers — consistent file organization"
  - "dtmfQueue size 10: sufficient for any realistic keypress burst; non-blocking send with drop+warn rather than blocking rtpReader"
  - "dtmfQueue case placed before ticker.C in wsPacer select: DTMF is control event, gets priority over 20ms audio pacing"
  - "fmt added to ws.go imports for Sprintf in sendDTMF sequenceNumber formatting"
  - "No new dependencies required: RFC 4733 parsing uses only stdlib byte operations on pion/rtp Packet.Payload"

patterns-established:
  - "DTMF forwarding: parse on rtpReader, dedup by RTP timestamp, enqueue to bounded channel, drain in wsPacer"
  - "RFC 4733 retransmit dedup: lastDtmfTS field compared per-packet before enqueue"

requirements-completed: [WSB-07]

# Metrics
duration: 2min
completed: 2026-03-04
---

# Phase 7 Plan 02: DTMF Pipeline Summary

**RFC 4733 telephone-event DTMF detection forwarded to WebSocket as Twilio `dtmf` events via dtmfQueue channel with timestamp-based retransmit dedup**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-04T14:57:00Z
- **Completed:** 2026-03-04T14:59:00Z
- **Tasks:** 2
- **Files modified:** 3

## Accomplishments

- parseTelephoneEvent parses RFC 4733 4-byte telephone-event payload: validates length, maps event codes 0-15 to "0"-"9"/"*"/"#"/"A"-"D", extracts End bit
- sendDTMF writes Twilio-compliant `dtmf` JSON event to WebSocket via existing writeJSON helper (sole WS writer invariant preserved)
- dtmfQueue (buffered, size 10) wired between rtpReader and wsPacer; non-blocking send prevents rtpReader from ever blocking on DTMF
- RFC 4733 retransmit deduplication: lastDtmfTS field drops End=1 packets with the same RTP timestamp (3x retransmit produces exactly 1 dtmfQueue entry)
- wsPacer drains dtmfQueue with higher priority than 20ms audio tick — dtmfQueue case appears before ticker.C in select

## Task Commits

Each task was committed atomically:

1. **Task 1: Add parseTelephoneEvent + DtmfEvent types + sendDTMF to ws.go** - `ab68731` (feat)
2. **Task 2: Wire DTMF pipeline — dtmfQueue in session.go (rtpReader + wsPacer)** - `397fe84` (feat)

**Plan metadata:** (docs commit — see final commit)

_Note: Both tasks used TDD (RED: failing tests committed first, GREEN: implementation passes)_

## Files Created/Modified

- `internal/bridge/ws.go` - Added parseTelephoneEvent, dtmfEventToDigit, DtmfEvent, DtmfBody, sendDTMF; added fmt import
- `internal/bridge/ws_test.go` - Added TestParseTelephoneEvent_{ShortPayload,EventCodeTooHigh,Digits,EndBit}, TestSendDTMF_JSONSchema, TestDTMFDeduplication_SameTimestamp, TestDTMFForwarding_NewTimestamp
- `internal/bridge/session.go` - Added lastDtmfTS + dtmfQueue fields to CallSession; dtmfQueue init in run(); rtpReader DTMF branch; wsPacer dtmfQueue case

## Decisions Made

- `parseTelephoneEvent` and `sendDTMF` placed in `ws.go` alongside other WS event helpers for consistent file organization
- `dtmfQueue` size 10: sufficient for any realistic keypress burst; non-blocking send with drop+warn rather than blocking rtpReader
- `dtmfQueue` case placed before `ticker.C` in wsPacer select: DTMF is a control event, gets priority over 20ms audio pacing
- `fmt` added to `ws.go` imports for Sprintf in sendDTMF sequenceNumber formatting
- No new dependencies: RFC 4733 parsing uses only stdlib byte operations on existing pion/rtp Packet.Payload field

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- DTMF pipeline complete and tested with -race; all 19 bridge tests pass
- Phase 7 (WebSocket Resilience + DTMF) now fully complete: 07-01 reconnect loop + 07-02 DTMF forwarding
- Ready for Phase 8: Production Hardening

---
*Phase: 07-websocket-resilience-dtmf*
*Completed: 2026-03-04*
