---
phase: 01-foundation
plan: 02
subsystem: sip
tags: [sip.js, websocket, ws-polyfill, registerer, registration, node.js]

# Dependency graph
requires:
  - phase: 01-01
    provides: "Zod 4 validated Config type, pino createChildLogger helper, NodeNext ESM scaffold"
provides:
  - "createSipUserAgent factory: ws polyfill, UserAgent + Registerer construction, stateChange listener, transport onConnect/onDisconnect handlers"
  - "SipHandle interface: { ua: UserAgent, registerer: Registerer }"
  - "Automatic re-registration via refreshFrequency 90 (re-registers at 90% of server-granted expiry)"
  - "Transport reconnect handling: onConnect unconditionally re-issues REGISTER"
  - "src/index.ts entrypoint: wires config + logger + createSipUserAgent, keeps process alive via SIP transport event loop"
affects: [02-foundation, 03-foundation, all-phases]

# Tech tracking
tech-stack:
  added:
    - "ws 8.19.0 polyfill — sets globalThis.WebSocket before SIP.js import (critical import order)"
    - "sip.js 0.21.2 UserAgent + Registerer API"
  patterns:
    - "ws-polyfill-first: ws must be imported and assigned to globalThis.WebSocket BEFORE any SIP.js import"
    - "Registerer refreshFrequency 90: automatic re-registration without manual timers (SIP-02)"
    - "transport.onConnect re-register: unconditional REGISTER on any WSS reconnect"
    - "async main() with fatal catch: startup errors logged and exit code 1"

key-files:
  created:
    - "src/sip/userAgent.ts"
  modified:
    - "src/index.ts"

key-decisions:
  - "Remove UserAgent.defaultTransportConstructor — SIP.js 0.21.x has no such static; defaulting omits the field (WebSocketTransport is used by default)"
  - "refreshFrequency 90 in Registerer — re-registers at 90% of server expiry without manual timer management"
  - "viaHost falls back to SIP_DOMAIN if SDP_CONTACT_IP not set — fails loudly at sipgate rather than silently timing out"
  - "No setInterval keepalive — SIP WebSocket transport keeps Node.js event loop alive naturally"

patterns-established:
  - "ws-polyfill-first: import ws and set globalThis.WebSocket before any SIP.js module loads"
  - "createSipUserAgent factory: receives Config + Logger, returns SipHandle; allows Phase 2 to pass call-scoped child loggers"
  - "Fatal startup via main().catch — all async startup failures surface to process.exit(1)"

requirements-completed: [SIP-01, SIP-02]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 1 Plan 02: SIP.js UserAgent Factory Summary

**SIP.js 0.21.x UserAgent + Registerer factory with ws globalThis polyfill, automatic re-registration at 90% expiry, and WSS transport reconnect handling for sipgate registration**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-03T10:15:17Z
- **Completed:** 2026-03-03T10:18:07Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- `createSipUserAgent` factory sets ws polyfill before SIP.js import, constructs UserAgent and Registerer from config, wires stateChange listener emitting `event:sip_registered` on 200 OK
- Automatic re-registration: `refreshFrequency: 90` causes Registerer to re-register at 90% of server-granted expiry without any manual timer code
- Transport reconnect: `ua.transport.onConnect` unconditionally calls `registerer.register()` ensuring REGISTER is re-issued after any WSS disconnect/reconnect cycle
- `src/index.ts` updated to async main() calling createSipUserAgent; SIP WebSocket transport keeps event loop alive; fatal errors exit code 1

## Task Commits

Each task was committed atomically:

1. **Task 1: SIP.js UserAgent factory with ws polyfill and Registerer** - `e19564a` (feat)
2. **Task 2: Wire entrypoint — src/index.ts calls createSipUserAgent** - `541897a` (feat)

## Files Created/Modified
- `src/sip/userAgent.ts` - createSipUserAgent factory: ws polyfill, UserAgent + Registerer, stateChange listener, transport callbacks, exports SipHandle interface
- `src/index.ts` - Updated entrypoint: imports createSipUserAgent, async main() keeps process alive via SIP transport event loop

## Decisions Made
- Omitted `transportConstructor` field: `UserAgent.defaultTransportConstructor` does not exist in SIP.js 0.21.x — omitting the field causes SIP.js to default to WebSocketTransport as documented
- Used `refreshFrequency: 90` on Registerer — handles SIP-02 re-registration automatically without manual timers
- `viaHost` falls back to `SIP_DOMAIN` when `SDP_CONTACT_IP` is absent — produces a meaningful failure at the sipgate registrar rather than a silent timeout
- No `setInterval` or artificial keep-alive in index.ts — the ws-backed SIP transport WebSocket keeps the Node.js event loop alive naturally

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] UserAgent.defaultTransportConstructor does not exist in SIP.js 0.21.x**
- **Found during:** Task 1 (SIP.js UserAgent factory) — typecheck step
- **Issue:** Plan specified `transportConstructor: UserAgent.defaultTransportConstructor` but `UserAgent` has no such static property in SIP.js 0.21.2; TypeScript error TS2339 blocked compilation
- **Fix:** Removed `transportConstructor` from `UserAgentOptions`; SIP.js defaults to `WebSocketTransport` when the field is absent (documented behavior)
- **Files modified:** `src/sip/userAgent.ts`
- **Verification:** `pnpm typecheck` exits 0; module loads and exports `createSipUserAgent` correctly
- **Committed in:** `e19564a` (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug — invalid SIP.js 0.21.x API reference)
**Impact on plan:** Fix necessary for typecheck to pass. The plan referenced a non-existent static; removing it achieves identical runtime behavior as WebSocketTransport is the default. No scope creep.

## Issues Encountered
- `src/sip/userAgent.ts` was already committed (in `e19564a`) before this plan execution, grouped with 01-03 docker infra commits. The file content was correct and already matched what this plan requires. No re-commit was needed for Task 1.

## User Setup Required
**External services require manual configuration before `pnpm dev` produces event:sip_registered.**
Required environment variables:
- `SIP_USER` — sipgate SIP-ID (e.g. `e12345p0`)
- `SIP_PASSWORD` — SIP password from sipgate portal
- `SIP_DOMAIN` — SIP registrar domain (verify from portal; MEDIUM confidence: `sipconnect.sipgate.de`)
- `SIP_REGISTRAR` — WSS URL (verify from portal; MEDIUM confidence: `wss://sip.sipgate.de`)
- `SDP_CONTACT_IP` — your externally reachable IP (required for SIP contact header)
- `WS_TARGET_URL` — WebSocket bridge URL (any valid URL for Phase 1 testing)

Verification: `pnpm dev 2>&1 | head -20` should produce a JSON line with `"event":"sip_registered"` within 10 seconds.

## Next Phase Readiness
- SIP REGISTER foundation complete — Phase 2 can import `createSipUserAgent` and `SipHandle` directly
- `SipHandle.ua` exposes the `UserAgent` for INVITE handler registration in Phase 2
- All structural patterns established: createChildLogger per component, transport event callbacks, SipHandle return type
- Blocker remains: sipgate WSS endpoint URLs at MEDIUM confidence — must be verified from sipgate portal before first live test

---
*Phase: 01-foundation*
*Completed: 2026-03-03*

## Self-Check: PASSED

- FOUND: src/sip/userAgent.ts
- FOUND: src/index.ts
- FOUND: .planning/phases/01-foundation/01-02-SUMMARY.md
- FOUND commit: e19564a (Task 1 — userAgent.ts)
- FOUND commit: 541897a (Task 2 — index.ts entrypoint)
