---
phase: 11-node.js-equivalents
plan: 01
subsystem: testing
tags: [vitest, typescript, tdd, mark, clear, options, sip]

# Dependency graph
requires:
  - phase: 10-go-sip-options-keepalive
    provides: applyOptionsResponse behavioral spec (OPTIONS 401/407 = server alive; only timeout/5xx/404 trigger re-register)
  - phase: 09-go-bridge-mark-clear
    provides: mark sentinel / sendClear behavioral spec (outboundDrain tagged union, sole-writer invariant)
provides:
  - Vitest 4.0.18 installed as devDependency in node/
  - "test": "vitest run" script in node/package.json
  - node/test/wsClient.mark.test.ts — 4 todo stubs for MRKN-01, MRKN-02, MRKN-03
  - node/test/userAgent.options.test.ts — 7 todo stubs for OPTN-01/02/03
affects: [11-02, 11-03]

# Tech tracking
tech-stack:
  added: [vitest@4.0.18]
  patterns: [it.todo stubs for TDD red baseline — pending, not failing, so Wave 1 CI stays green]

key-files:
  created:
    - node/test/wsClient.mark.test.ts
    - node/test/userAgent.options.test.ts
  modified:
    - node/package.json
    - node/pnpm-lock.yaml

key-decisions:
  - "it.todo stubs chosen over failing expect() stubs — pnpm test exits 0 (green pending) so Wave 1 CI can run without false-red on infrastructure tests"
  - "No vitest.config.ts needed — Vitest auto-discovers node/test/**/*.test.ts in a type:module pnpm project"

patterns-established:
  - "TDD red baseline: write it.todo stubs first, run test suite (exits 0), then add real failing expect() in implementation plan"
  - "vitest run (not vitest watch) for CI-safe test execution"

requirements-completed: [MRKN-01, MRKN-02, MRKN-03, OPTN-01, OPTN-02, OPTN-03]

# Metrics
duration: 1min
completed: 2026-03-05
---

# Phase 11 Plan 01: Node.js TDD Red Baseline Summary

**Vitest 4.0.18 installed in node/ with 11 pending it.todo stubs covering MRKN-01/02/03 and OPTN-01/02/03 — pnpm test exits 0**

## Performance

- **Duration:** ~1 min
- **Started:** 2026-03-05T15:55:27Z
- **Completed:** 2026-03-05T15:56:16Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Vitest 4.0.18 installed as devDependency; `"test": "vitest run"` added to node/package.json scripts
- `node/test/wsClient.mark.test.ts` created with 4 todo stubs (MRKN-01 x2, MRKN-02, MRKN-03)
- `node/test/userAgent.options.test.ts` created with 7 todo stubs (all OPTN-01/02/03 cases)
- `pnpm test` exits 0 with 11 pending todos — Vitest discovers both files via auto-discovery

## Task Commits

Each task was committed atomically:

1. **Task 1: Install Vitest and add test script** - `0d7fc84` (chore)
2. **Task 2: Write failing test stubs (red) for mark/clear and OPTIONS** - `d6ad0fa` (test)

## Files Created/Modified
- `node/package.json` - Added vitest devDependency + "test": "vitest run" script
- `node/pnpm-lock.yaml` - Updated lockfile after vitest install
- `node/test/wsClient.mark.test.ts` - 4 todo stubs: MRKN-01 (dequeue+fast-path), MRKN-02 (sendClear), MRKN-03 (sendMark JSON)
- `node/test/userAgent.options.test.ts` - 7 todo stubs: 200 OK, 1st/2nd failure, 401, 407, 404, 500

## Decisions Made
- Used `it.todo()` rather than `it('...', () => { expect(...).toBe(...) })` — todos count as pending (not failures), so `pnpm test` exits 0. This keeps CI green during the red baseline phase and matches the plan specification.
- No `vitest.config.ts` required — Vitest's auto-discovery handles `type: "module"` pnpm projects without extra configuration.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Vitest infrastructure is ready for Plans 02 and 03 to add real failing `expect()` tests alongside implementation
- Plan 02 will convert wsClient.mark.test.ts todos to real failing tests + implement mark/clear in wsClient.ts
- Plan 03 will convert userAgent.options.test.ts todos to real failing tests + implement applyOptionsResponse

---
*Phase: 11-node.js-equivalents*
*Completed: 2026-03-05*
