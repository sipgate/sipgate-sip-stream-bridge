---
phase: 09-go-bridge-mark-clear
plan: "01"
subsystem: api
tags: [go, rtp, websocket, prometheus, bridge, twilio-media-streams]

# Dependency graph
requires:
  - phase: 08-lifecycle-observability
    provides: Metrics struct with custom Prometheus registry; CallSession lifecycle with goroutine model
provides:
  - outboundFrame tagged union type in go/internal/bridge/session.go
  - packetQueue typed as chan outboundFrame (no more chan []byte)
  - markEchoQueue chan string (cap 10) on CallSession
  - clearSignal chan struct{} (cap 1) on CallSession
  - MarkEchoed prometheus.Counter (mark_echoed_total)
  - ClearReceived prometheus.Counter (clear_received_total)
affects:
  - 09-02: Plan 02 adds mark sentinel routing through markEchoQueue and clearSignal drain logic — depends on all fields from this plan
  - 09-03: Plan 03 wsPacer mark echo send — depends on markEchoQueue

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Tagged union channel type: outboundFrame{audio, mark} replaces bare []byte — discriminate at consumer site"
    - "Channel initialization before wg.Add: all queues created atomically in run() before goroutine launch"

key-files:
  created: []
  modified:
    - go/internal/bridge/session.go
    - go/internal/observability/metrics.go

key-decisions:
  - "outboundFrame as tagged union: separate audio and mark fields rather than encoding mark as special []byte sentinel — avoids magic values, makes nil check for silence fall-back idiomatic"
  - "Plan 01 leaves mark routing unimplemented intentionally: rtpPacer treats mark frames as audio (frame.audio == nil -> silence fallback) until Plan 02 adds routing — enables incremental, always-compiling changes"
  - "clearSignal buffered(1): single-slot buffer prevents wsToRTP from blocking if rtpPacer has not yet consumed the previous signal"

patterns-established:
  - "outboundFrame tagged union: use for all future packetQueue producers/consumers — check audio != nil for RTP send, mark != '' for echo routing"
  - "Queue capacity convention: markEchoQueue=10, clearSignal=1 (matches plan spec, do not change without design review)"

requirements-completed: []  # MARK-01 through MARK-04 are listed in plan frontmatter but the behavioral implementation comes in Plans 02+03; this plan only provides type scaffolding (no behavioral change)

# Metrics
duration: 2min
completed: 2026-03-05
---

# Phase 09 Plan 01: Go Bridge Type Foundation Summary

**outboundFrame tagged union replaces chan []byte packetQueue, adding markEchoQueue + clearSignal channels and mark_echoed_total + clear_received_total Prometheus counters — compile-clean with zero behavioral change**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-05T10:47:05Z
- **Completed:** 2026-03-05T10:49:06Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments

- Introduced `outboundFrame` tagged union type enabling mark/audio discrimination at the rtpPacer consumer
- Renamed `packetQueue` from `chan []byte` to `chan outboundFrame` with all producers and consumers updated
- Added `markEchoQueue chan string` and `clearSignal chan struct{}` to CallSession, initialized in run() before goroutine launch
- Registered `mark_echoed_total` and `clear_received_total` Prometheus counters on the custom registry

## Task Commits

Each task was committed atomically:

1. **Task 1: Rename packetQueue type and add new CallSession channel fields** - `1001803` (feat)
2. **Task 2: Add mark_echoed_total and clear_received_total Prometheus counters** - `bb8b0f7` (feat)

**Plan metadata:** (docs commit — see final_commit below)

## Files Created/Modified

- `go/internal/bridge/session.go` — Added outboundFrame type; changed packetQueue to chan outboundFrame; added markEchoQueue + clearSignal fields and make() calls; updated wsToRTP producer and rtpPacer consumer
- `go/internal/observability/metrics.go` — Added MarkEchoed + ClearReceived fields; created and registered mark_echoed_total + clear_received_total counters

## Decisions Made

- Chose tagged union (separate `audio []byte` and `mark string` fields) over encoding marks as magic byte values — idiomatic Go nil check for silence fallback, no coupling between encoding and routing
- Deliberately left mark sentinel routing unimplemented in Plan 01 (rtpPacer falls back to silence on nil frame.audio) — ensures codebase always compiles between plan commits

## Deviations from Plan

None - plan executed exactly as written.

The plan noted that `go build ./go/...` should be run from the project root — but the Go module lives in `go/`, so the commands were run as `cd go && go build ./...`. The plan's verify commands had this same structure conceptually; no deviation in intent.

## Issues Encountered

The plan's verification command `go build ./go/...` must be run from inside the `go/` directory (`cd go && go build ./...`) because the Go module root is `go/go.mod`. Running from the repo root fails with "directory prefix go does not contain main module". This is a known project layout — handled correctly.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Plan 02 can proceed immediately: all type scaffolding is in place
- `markEchoQueue` and `clearSignal` are initialized and ready for routing logic
- `MarkEchoed` and `ClearReceived` counters are registered and ready for `.Inc()` calls
- No blockers

---
*Phase: 09-go-bridge-mark-clear*
*Completed: 2026-03-05*

## Self-Check: PASSED

All files, commits, and code patterns verified:
- go/internal/bridge/session.go: FOUND
- go/internal/observability/metrics.go: FOUND
- 09-01-SUMMARY.md: FOUND
- Commit 1001803 (Task 1): FOUND
- Commit bb8b0f7 (Task 2): FOUND
- type outboundFrame struct: FOUND
- chan outboundFrame: FOUND
- markEchoQueue: FOUND
- clearSignal: FOUND
- MarkEchoed: FOUND
- ClearReceived: FOUND
- mark_echoed_total: FOUND
- clear_received_total: FOUND
