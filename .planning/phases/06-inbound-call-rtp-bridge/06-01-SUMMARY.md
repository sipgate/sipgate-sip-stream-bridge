---
phase: 06-inbound-call-rtp-bridge
plan: 01
subsystem: sip
tags: [sipgo, pion-sdp, sip, invite, ack, bye, options, sdp, dtmf, rtp, tdd]

# Dependency graph
requires:
  - phase: 05-sip-registration
    provides: "Agent struct (UA+Server+Client), agent.Server for handler registration, pion/sdp v3.0.18 in go.mod"
  - phase: 04-go-scaffold
    provides: "Config struct with SDPContactIP, RTPPortMin, RTPPortMax, SIPUser fields"

provides:
  - "internal/sip/sdp.go — CallerSDP type + ParseCallerSDP (SDP offer parsing) + BuildSDPAnswer (200 OK SDP)"
  - "internal/sip/sdp_test.go — 6 TDD tests covering DTMF PT extraction, PCMU-only fallback, session-level c=, error paths"
  - "internal/sip/handler.go — Handler struct + NewHandler + onInvite/onAck/onBye/onOptions; CallManagerIface"

affects: [06-02-bridge-manager, 06-03-websocket-bridge, 08-lifecycle]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "pion/sdp/v3 Unmarshal + GetCodecForPayloadType for DTMF PT extraction from SDP offer"
    - "sipgo.DialogServerCache wired to agent.Server for UAS dialog state machine"
    - "CallManagerIface defined in sip package to avoid circular import with bridge package"
    - "onInvite launches StartSession in goroutine so handler returns immediately for next INVITE"
    - "Port released explicitly on RespondSDP error path; otherwise released inside StartSession"

key-files:
  created:
    - "internal/sip/sdp.go — ParseCallerSDP and BuildSDPAnswer; CallerSDP type"
    - "internal/sip/sdp_test.go — 6 TDD tests (red-green)"
    - "internal/sip/handler.go — INVITE/ACK/BYE/OPTIONS handlers + DialogServerCache; CallManagerIface"
  modified: []

key-decisions:
  - "CallManagerIface defined in sip package (not bridge) — avoids circular import when bridge package imports sip types"
  - "StartSession launched as goroutine in onInvite — ensures handler returns immediately; prevents blocking next INVITE (Open Question 1 from RESEARCH.md)"
  - "DTMFPayloadType never hardcoded — always extracted from SDP offer; fallback 101 if no telephone-event found"
  - "SDPContactIP used in both contact header and BuildSDPAnswer — container IP never leaks into SDP"

patterns-established:
  - "Pattern: ParseCallerSDP checks per-media c= first, falls back to session-level c= (both SDP layouts supported)"
  - "Pattern: onInvite error paths return after dlg.Respond(503) — port released before any error return"
  - "Pattern: NewHandler registers all four SIP methods before registrar.Register() is called"

requirements-completed: [SIP-03, SIP-04, SIP-05]

# Metrics
duration: 2min
completed: 2026-03-04
---

# Phase 6 Plan 01: Inbound Call RTP Bridge Summary

**pion/sdp-based SDP offer parsing (DTMF PT 113 extraction) + sipgo DialogServerCache INVITE/ACK/BYE/OPTIONS handler with CallManagerIface for bridge decoupling**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-04T06:59:41Z
- **Completed:** 2026-03-04T07:02:08Z
- **Tasks:** 2
- **Files modified:** 3 created

## Accomplishments

- TDD cycle complete: 6 RED tests written first, sdp.go implementation made all GREEN
- ParseCallerSDP correctly extracts DTMF PT 113 from sipgate-style SDP offers; falls back to 101
- BuildSDPAnswer produces valid RFC 4566 SDP with PCMU PT 0, mirrored DTMF PT, a=sendrecv, cfg.SDPContactIP
- handler.go implements full UAS dialog flow using sipgo.DialogServerCache (ReadInvite/RespondSDP/ReadAck/ReadBye)
- CallManagerIface defined in sip package to avoid circular imports with the bridge package (06-02)
- INVITE handler launches StartSession goroutine so it does not block subsequent INVITE handling

## Task Commits

Each task was committed atomically:

1. **Task 1 RED: Failing TDD tests for ParseCallerSDP and BuildSDPAnswer** - `ceb6cad` (test)
2. **Task 1 GREEN: Implement ParseCallerSDP and BuildSDPAnswer** - `c702950` (feat)
3. **Task 2: SIP INVITE/ACK/BYE/OPTIONS handler** - `b67d5d8` (feat)

**Plan metadata:** _(docs commit follows)_

_Note: TDD Task 1 has two commits (RED test → GREEN implementation)_

## Files Created/Modified

- `internal/sip/sdp.go` - CallerSDP type; ParseCallerSDP (pion/sdp Unmarshal + DTMF PT scan); BuildSDPAnswer (PCMU+telephone-event, sendrecv, SDPContactIP in o= and c=)
- `internal/sip/sdp_test.go` - 6 TDD tests: PT 113 extraction, PT 101 fallback, session-level c= fallback, no-audio-section error, PCMU+DTMF in output, sendrecv attribute
- `internal/sip/handler.go` - Handler struct; NewHandler (DialogServerCache + four handler registrations); onInvite (ReadInvite→ParseCallerSDP→AcquirePort→100→RespondSDP→goroutine); onAck/onBye (ReadAck/ReadBye); onOptions (200 OK); CallManagerIface

## Decisions Made

- **CallManagerIface in sip package:** bridge package will satisfy this interface, but defining it in sip avoids a circular import (sip would import bridge, bridge imports sip for CallerSDP).
- **goroutine for StartSession:** Research Open Question 1 — sipgo OnInvite runs in a per-transaction goroutine, but blocking it for the full call duration could prevent concurrent INVITE handling. Goroutine launch is the safe approach.
- **Port released on RespondSDP failure:** Prevents port pool exhaustion when transport errors occur after port acquisition (Pitfall 4 from RESEARCH.md).

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all APIs verified and used as documented.

## User Setup Required

None - no external service configuration required for this phase.

## Next Phase Readiness

- `internal/sip/handler.go` exports `NewHandler(agent, callManager, cfg, log)` — main.go can wire this in 06-03
- `CallManagerIface` is ready for 06-02 (bridge.CallManager) to satisfy
- `CallerSDP` type is exported and available for bridge.StartSession signature
- No new go.mod changes required — all dependencies were already present from Phase 5

---
*Phase: 06-inbound-call-rtp-bridge*
*Completed: 2026-03-04*
