---
phase: 05-sip-registration
plan: 01
subsystem: sip
tags: [sipgo, sip, register, digest-auth, zerolog, pion, rtp, sdp]

# Dependency graph
requires:
  - phase: 04-go-scaffold
    provides: "Config struct (SIPUser/SIPPassword/SIPDomain/SIPRegistrar/SIPExpires), zerolog logger setup, main.go scaffold, Docker image"

provides:
  - "internal/sip/agent.go — Agent struct (UA+Server+Client) + NewAgent constructor"
  - "internal/sip/registrar.go — Registrar struct with Register/doRegister/reregisterLoop/Unregister"
  - "cmd/audio-dock/main.go — SIP wiring: listeners started before Register, blocking Register call, SIGTERM/SIGINT shutdown"
  - "go.mod — emiago/sipgo@v1.2.0, pion/sdp/v3@v3.0.18, pion/rtp@v1.10.1, samber/slog-zerolog@v1.0.0"

affects: [06-call-handling, 07-audio-bridge, 08-lifecycle]

# Tech tracking
tech-stack:
  added:
    - "github.com/emiago/sipgo@v1.2.0 — SIP UA, REGISTER request, Digest Auth, Server/Client transport"
    - "github.com/pion/sdp/v3@v3.0.18 — Phase 6 readiness (SDP parsing)"
    - "github.com/pion/rtp@v1.10.1 — Phase 6 readiness (RTP packet handling)"
    - "github.com/samber/slog-zerolog@v1.0.0 — bridges sipgo slog output into zerolog JSON stream"
  patterns:
    - "sipgo UA shared between UAS (Server) and UAC (Client) — single transport for both inbound and outbound"
    - "Two-step REGISTER: Client.Do (no auth) → DoDigestAuth on 401/407 challenge"
    - "ClientRequestRegisterBuild option passed to Do — sets CSeq, clears userinfo per RFC 3261 §10.2"
    - "Server listeners started in goroutines BEFORE registrar.Register() — transport must be ready first"
    - "reregisterLoop calls doRegister, not Register — prevents goroutine nesting (Pitfall 6)"
    - "Re-register ticker at 75% of server-granted Expires (not requested Expires)"
    - "nil client guard in doRegister — returns error instead of panicking"

key-files:
  created:
    - "internal/sip/agent.go — Agent struct + NewAgent; sipgo UA/Server/Client creation with slog-zerolog bridge"
    - "internal/sip/registrar.go — Registrar: Register/doRegister/reregisterLoop/Unregister"
    - "internal/sip/registrar_test.go — 5 unit tests (nil client, 403 message, Expires fallback, ticker math)"
  modified:
    - "cmd/audio-dock/main.go — Phase 4 scaffold replaced with Phase 5 SIP wiring"
    - "go.mod / go.sum — sipgo, pion/sdp, pion/rtp, slog-zerolog added"

key-decisions:
  - "75% re-register interval matches diago's calcRetry pattern (not 90%) — confirmed from diago register_transaction.go source"
  - "nil client guard added in doRegister (Rule 1 fix) — sipgo Client.Do panics on nil receiver; guard returns proper error"
  - "Server.Close() not used for shutdown — returns nil in sipgo v1.2.0; actual shutdown via ctx cancel + ua.Close()"
  - "slog-zerolog bridge included — ensures sipgo internal logs flow as zerolog JSON, not fragmented slog output"
  - "pion/sdp and pion/rtp added in this phase despite not being used — avoids go.mod changes mid Phase 6 sprint"

patterns-established:
  - "Pattern: All sipgo-internal logs bridged through samber/slog-zerolog into zerolog JSON stream"
  - "Pattern: Transport listeners (UDP+TCP :5060) always started before Register call"
  - "Pattern: doRegister is the unit of work; Register and reregisterLoop both call doRegister (not each other)"

requirements-completed: [SIP-01, SIP-02]

# Metrics
duration: 3min
completed: 2026-03-03
---

# Phase 5 Plan 01: SIP Registration Summary

**sipgo v1.2.0 REGISTER+Digest Auth with 75% re-register loop; UDP/TCP transport listeners wired into main.go**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-03T22:26:37Z
- **Completed:** 2026-03-03T22:29:39Z
- **Tasks:** 3
- **Files modified:** 5

## Accomplishments

- Created `internal/sip/` package with Agent (UA+Server+Client) and Registrar (REGISTER lifecycle)
- Implemented full Digest Auth flow: Client.Do → 401 → DoDigestAuth → 200 OK + Expires extraction
- Re-register goroutine fires at 75% of server-granted Expires; sends UNREGISTER (Expires:0) on shutdown
- Wired SIP into main.go: listeners before Register, non-zero exit on registration failure (SIP-01)
- Added pion/sdp and pion/rtp for Phase 6 readiness without mid-sprint go.mod changes

## Task Commits

Each task was committed atomically:

1. **Task 1: Add sipgo deps + internal/sip/agent.go** - `8813b39` (feat)
2. **Task 2 RED: Failing Registrar unit tests** - `71af332` (test)
3. **Task 2 GREEN: Registrar implementation** - `4b43835` (feat)
4. **Task 3: Wire SIP into main.go** - `43171e4` (feat)

**Plan metadata:** _(docs commit follows)_

_Note: TDD Task 2 has two commits (RED test → GREEN implementation)_

## Files Created/Modified

- `internal/sip/agent.go` - Agent struct; NewAgent creates UA/Server/Client with slog-zerolog bridge
- `internal/sip/registrar.go` - Registrar; Register/doRegister/reregisterLoop/Unregister
- `internal/sip/registrar_test.go` - 5 unit tests covering nil client, 403 error, Expires fallback, ticker math
- `cmd/audio-dock/main.go` - Phase 4 scaffold replaced with Phase 5 SIP wiring
- `go.mod` / `go.sum` - sipgo@v1.2.0, pion/sdp/v3@v3.0.18, pion/rtp@v1.10.1, slog-zerolog@v1.0.0 added

## Decisions Made

- **75% re-register interval:** Matches diago's `calcRetry` ratio (confirmed from source). SIP-02 is satisfied.
- **Server.Close() not called:** Returns nil in sipgo v1.2.0. Shutdown via ctx cancel + `agent.UA.Close()` via defer.
- **slog-zerolog bridge added:** Ensures sipgo's `log/slog` internal logs appear as zerolog JSON on stdout, not fragmented.
- **pion libs added early:** Avoids go.mod changes mid Phase 6 sprint (per RESEARCH.md recommendation).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Added nil client guard in doRegister**
- **Found during:** Task 2 GREEN (registrar test run)
- **Issue:** sipgo `Client.Do` panics with a nil pointer dereference when called on a nil `*sipgo.Client`; Test 1 expected an error, not a panic
- **Fix:** Added `if r.client == nil { return 0, fmt.Errorf("REGISTER send: sipgo client is nil") }` at the start of `doRegister`
- **Files modified:** `internal/sip/registrar.go`
- **Verification:** `go test ./internal/sip/... -run TestRegistrar_NilClientReturnsError` passes
- **Committed in:** `4b43835` (Task 2 GREEN commit)

---

**Total deviations:** 1 auto-fixed (1 bug — nil pointer guard)
**Impact on plan:** Essential for test correctness and production robustness. No scope creep.

## Issues Encountered

None — all issues resolved inline per deviation rules.

## User Setup Required

None - no external service configuration required for this phase.
Live integration test (real sipgate credentials) is explicitly out of scope for Phase 5 plans.

## Next Phase Readiness

- SIP UA layer complete: `sip.NewAgent` + `sip.NewRegistrar` exported and usable by Phase 6
- `agent.Server` ready for `OnRequest(INVITE, ...)` handler registration in Phase 6
- `agent.Client` available for outbound SIP requests (BYE, etc.) in Phase 6/8
- pion/sdp and pion/rtp already in go.mod — Phase 6 can start coding immediately
- Blocker noted in STATE.md: sipgate server-granted Expires value unconfirmed until live integration test — log `server_expires_s` field will reveal actual value

---
*Phase: 05-sip-registration*
*Completed: 2026-03-03*
