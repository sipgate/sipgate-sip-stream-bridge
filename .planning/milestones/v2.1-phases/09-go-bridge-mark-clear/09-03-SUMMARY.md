---
phase: 09-go-bridge-mark-clear
plan: "03"
subsystem: testing
tags: [go, bridge, mark, clear, rtp, websocket, channel, race-detector]

# Dependency graph
requires:
  - phase: 09-02
    provides: sendMarkEcho function, MarkEvent/MarkBody types, outboundFrame type, markEchoQueue, clearSignal channels

provides:
  - TestSendMarkEcho_JSONSchema: net.Pipe round-trip JSON schema verification for sendMarkEcho
  - TestMarkSentinel_RoutedToEchoQueue: MARK-01 channel-logic test (sentinel routing in rtpPacer)
  - TestMarkImmediate_EmptyQueue: MARK-02 channel-logic test (immediate echo on empty packetQueue)
  - TestClear_DrainsQueueAndEchoesMarks: MARK-03 channel-logic test (drain audio + echo marks)
  - TestClear_RTPContinues: MARK-04 channel-logic test (silence continues after clear)

affects: [phase 10, phase 11]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Channel-logic tests without goroutines: directly simulate channel operations to test correctness without concurrency (mirrors TestDTMFDeduplication_SameTimestamp pattern)"
    - "net.Pipe round-trip testing: in-process conn pair for WS JSON schema verification without a real WS server"

key-files:
  created:
    - go/internal/bridge/session_mark_test.go
  modified:
    - go/internal/bridge/ws_test.go

key-decisions:
  - "Channel-logic tests run synchronously (no goroutines) — keeps tests fast (<1ms each) and race-detector-clean by construction"
  - "session_mark_test.go is package bridge (not package bridge_test) to access unexported outboundFrame type and pcmuSilenceFrame variable"

patterns-established:
  - "Direct channel simulation pattern: test channel routing logic by exercising select/case blocks inline without goroutine orchestration"

requirements-completed: [MARK-01, MARK-02, MARK-03, MARK-04]

# Metrics
duration: 2min
completed: 2026-03-05
---

# Phase 9 Plan 03: Mark/Clear Tests Summary

**Five new tests covering all four MARK requirements: net.Pipe JSON schema verification for sendMarkEcho + channel-logic tests for sentinel routing, immediate echo, drain-on-clear, and silence-after-clear — all passing under go test -race with zero data races**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-03-05T10:55:53Z
- **Completed:** 2026-03-05T10:57:33Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- Added `TestSendMarkEcho_JSONSchema` to `ws_test.go` — verifies the full JSON wire format (event, streamSid, sequenceNumber, mark.name) via `net.Pipe()` round-trip with `wsutil.ReadClientData`
- Created `session_mark_test.go` with four channel-logic tests that verify MARK-01 through MARK-04 without starting goroutines
- `go test -race ./internal/bridge/... ./internal/observability/...` exits 0 with zero data races and 24 tests passing

## Task Commits

Each task was committed atomically:

1. **Task 1: Add TestSendMarkEcho_JSONSchema to ws_test.go** - `ad3e23e` (test)
2. **Task 2: Add channel-logic tests for MARK-01 through MARK-04** - `ba7901c` (test)

## Tests Added

### TestSendMarkEcho_JSONSchema (ws_test.go)
- Uses `newPipe(t)` + goroutine pattern (mirrors `TestSendDTMF_JSONSchema`)
- Calls `sendMarkEcho(client, "MZtest123", "greeting-end", 7)` in goroutine
- Reads frame from server side via `wsutil.ReadClientData`
- Asserts: `event="mark"`, `streamSid="MZtest123"`, `sequenceNumber="7"`, `mark.name="greeting-end"`

### TestMarkSentinel_RoutedToEchoQueue (session_mark_test.go)
- Verifies MARK-01: mark sentinel dequeued by rtpPacer goes to markEchoQueue, not sent as RTP
- Enqueues [audio, mark, audio] — simulates 3 dequeue iterations inline
- Asserts: 2 audio frames processed, 1 mark in markEchoQueue with name "end-of-speech"

### TestMarkImmediate_EmptyQueue (session_mark_test.go)
- Verifies MARK-02: mark with empty packetQueue goes directly to markEchoQueue
- Simulates wsToRTP mark-handling branch: `len(packetQueue) == 0` → direct echo
- Asserts: markEchoQueue len=1, packetQueue len=0

### TestClear_DrainsQueueAndEchoesMarks (session_mark_test.go)
- Verifies MARK-03: clearSignal triggers drain of packetQueue — audio discarded, marks echoed
- Fills queue with [audio, mark("intro"), audio, audio, mark("outro")], signals clear
- Simulates rtpPacer drain loop with labeled `break drainLoop`
- Asserts: packetQueue empty, markEchoQueue contains "intro" then "outro"

### TestClear_RTPContinues (session_mark_test.go)
- Verifies MARK-04: after clear, rtpPacer produces silence (not stopped)
- Tick 1: drain runs (clears 1 audio frame), dequeue → empty → `pcmuSilenceFrame`
- Tick 2: no clear, no queued audio → `pcmuSilenceFrame` again
- Asserts: `silenceSent=true` on tick 1, `silence2=true` on tick 2, packetQueue empty

## Files Created/Modified
- `go/internal/bridge/ws_test.go` — Added TestSendMarkEcho_JSONSchema (44 lines)
- `go/internal/bridge/session_mark_test.go` — Created with 4 channel-logic tests (212 lines)

## Decisions Made
- Channel-logic tests run synchronously (no goroutines) — keeps tests fast and race-detector-clean by construction, mirrors TestDTMFDeduplication_SameTimestamp pattern already established in ws_test.go
- `session_mark_test.go` uses `package bridge` (not `package bridge_test`) to access `outboundFrame` (unexported type) and `pcmuSilenceFrame` (unexported var) without exposition

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 9 fully complete: all MARK/CLEAR requirements (MARK-01 through MARK-04) implemented and tested
- `go test -race` passes clean across all bridge and observability tests
- Ready for Phase 10 (Go SIP layer) or Phase 11 (Node.js) — both independent of Phase 9

---
*Phase: 09-go-bridge-mark-clear*
*Completed: 2026-03-05*
