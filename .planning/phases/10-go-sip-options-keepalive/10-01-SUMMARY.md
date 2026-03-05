---
phase: 10-go-sip-options-keepalive
plan: 01
subsystem: sip
tags: [go, prometheus, config, keepalive, options]

# Dependency graph
requires:
  - phase: 09-go-bridge-mark-clear
    provides: outboundFrame type, MarkEchoed/ClearReceived counters — config and metrics patterns established there
provides:
  - "SIPOptionsInterval time.Duration field on config.Config (env:SIP_OPTIONS_INTERVAL, default:30s)"
  - "SIPOptionsFailures prometheus.Counter on observability.Metrics (sip_options_failures_total)"
affects:
  - 10-02-go-sip-options-keepalive
  - sip/registrar.go (Plan 02 will wire SIPOptionsInterval and SIPOptionsFailures.Inc())

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "time.Duration env field with default string (e.g. default:\"30s\") — go-simpler.org/env v0.12.0 supports this natively"
    - "TDD scaffold approach: add field to struct + register counter before wiring to any consumer — codebase always compiles between commits"

key-files:
  created:
    - go/internal/config/config_test.go (new tests added to existing file)
    - go/internal/observability/metrics_test.go
  modified:
    - go/internal/config/config.go
    - go/internal/observability/metrics.go

key-decisions:
  - "Pure scaffold approach: add public contract definitions (field + counter) without wiring to registrar.go — Plan 02 will do the wiring, keeping build always-green"
  - "time import added to config.go for time.Duration field type"
  - "SIPOptionsFailures placed after ClearReceived in Metrics struct to follow existing counter ordering"

patterns-established:
  - "TDD order: failing test commit -> implementation commit (no separate refactor needed for pure additions)"
  - "Observability tests use observability_test package (black-box) matching the config_test convention"

requirements-completed:
  - OPTS-05
  - OPTS-01

# Metrics
duration: 2min
completed: 2026-03-05
---

# Phase 10 Plan 01: SIPOptionsInterval Config Field and SIPOptionsFailures Counter

**SIP OPTIONS keepalive scaffold: SIPOptionsInterval time.Duration (default 30s) and sip_options_failures_total Prometheus counter added as compile-safe definitions for Plan 02 to wire**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-03-05T13:15:47Z
- **Completed:** 2026-03-05T13:17:49Z
- **Tasks:** 2 (each with TDD red/green cycle)
- **Files modified:** 4

## Accomplishments
- Added `SIPOptionsInterval time.Duration` to `config.Config` with `env:"SIP_OPTIONS_INTERVAL"` and `default:"30s"` — parsed natively by go-simpler.org/env v0.12.0
- Added `SIPOptionsFailures prometheus.Counter` to `observability.Metrics` — created as `sip_options_failures_total`, registered on custom registry in `NewMetrics()`
- New test files with 4 passing tests covering default/override behavior and nil-safety/panic-free counter

## Task Commits

Each task was committed atomically:

1. **Task 1 RED: SIPOptionsInterval failing tests** - `cd848eb` (test)
2. **Task 1 GREEN: SIPOptionsInterval implementation** - `c47faea` (feat)
3. **Task 2 RED: SIPOptionsFailures failing tests** - `a5d03ec` (test)
4. **Task 2 GREEN: SIPOptionsFailures implementation** - `8a20d5e` (feat)

_Note: TDD tasks have two commits each (test RED -> feat GREEN). No refactor pass needed for pure struct/counter additions._

## Files Created/Modified
- `go/internal/config/config.go` - Added `SIPOptionsInterval time.Duration` field after `SIPExpires`; added `"time"` import
- `go/internal/config/config_test.go` - Added `TestLoad_SIPOptionsInterval_DefaultIs30s` and `TestLoad_SIPOptionsInterval_Override1m`
- `go/internal/observability/metrics.go` - Added `SIPOptionsFailures` field to struct, counter creation, `MustRegister`, and return literal
- `go/internal/observability/metrics_test.go` - Created with `TestNewMetrics_SIPOptionsFailures_NotNil` and `TestNewMetrics_SIPOptionsFailures_IncDoesNotPanic`

## Decisions Made
- Pure scaffold approach: add public-contract definitions without wiring to `registrar.go` — Plan 02 consumes these. Keeps build always-green between plan commits.
- `time.Duration` field type works with go-simpler.org/env v0.12.0's default string parser (e.g. `"30s"` parses to 30 * time.Second).

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- `config.Config.SIPOptionsInterval` is ready for Plan 02 to pass into `NewRegistrar`
- `metrics.Metrics.SIPOptionsFailures` is ready for Plan 02 to call `.Inc()` in `optionsKeepaliveLoop`
- `go build ./go/...` and all tests pass — codebase is green

---
*Phase: 10-go-sip-options-keepalive*
*Completed: 2026-03-05*
