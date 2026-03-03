# Project Retrospective: audio-dock

*A living document updated after each milestone. Lessons feed forward into future planning.*

---

## Milestone: v1.0 — MVP

**Shipped:** 2026-03-03
**Phases:** 3 | **Plans:** 10 | **Timeline:** 1 day

### What Was Built

- TypeScript/ESM project scaffold with Zod 4 fail-fast config validation and pino structured logging
- SIP.js registration against sipgate trunking with automatic re-registration (90% expiry refresh)
- Multi-stage Docker image on node:22-alpine with pnpm fetch layer caching
- Bidirectional RTP↔WebSocket bridge implementing full Twilio Media Streams protocol (connected/start/media/stop/dtmf)
- CallManager orchestrating concurrent SIP INVITE lifecycles with per-call session isolation
- WebSocket reconnect loop (exponential backoff 1s/2s/4s, 30s budget) with silence injection during gap
- Zero FD leak verified over 20 sequential calls via standalone ESM test script

### What Worked

- **Dependency-injection SipCallbacks pattern**: decoupling SIP dispatch from CallManager boundaries meant Phase 2 and Phase 3 work never stepped on each other
- **Per-call factory pattern** (createRtpHandler, createWsClient): each call gets fresh isolated instances — concurrent isolation was trivially correct
- **Phase ordering** (Foundation → Core Bridge → Resilience): each phase left a clean API surface for the next; no backtracking required
- **pnpm fetch layer in Dockerfile**: build cache hits on every source change, only invalidates on lockfile change
- **Drop-not-buffer policy**: deciding early to drop RTP during reconnect (not buffer) made reconnect logic simple and memory-bounded

### What Was Inefficient

- **ROADMAP tracking stale**: Plan checkboxes in ROADMAP.md were not updated as plans completed — roadmap analyze reported Phase 1 as "in progress" throughout. Need to update checkboxes at plan completion.
- **SIP.js quirks required research**: no official Node.js transport documentation meant several discovery iterations (ws polyfill import order, viaHost timing, refreshFrequency API)
- **Multiple SIP bug-fix commits**: INVITE handling required 4 post-implementation fix commits (Record-Route, Via echo, retransmit 200 OK, CANCEL handling) — SIP RFC edge cases not fully covered in initial plan

### Patterns Established

- **ws-polyfill-first**: `globalThis.WebSocket = ws` must be assigned before any SIP.js import — order matters
- **Registerer refreshFrequency 90**: use this for automatic re-registration, no manual timers needed
- **session.ws routing for RTP callbacks**: wire RTP event handlers through `session.ws` (not closure-captured ws) so reconnect transparently replaces the client
- **node --import tsx/esm**: run TypeScript sources directly from .mjs test scripts without a build step
- **Distinct port ranges for test vs prod**: use 20000-20099 for test, 10000-10099 for production — no port conflicts when service runs during test

### Key Lessons

1. **SIP RFC compliance is non-trivial**: Plan for SIP INVITE handling to require multiple correction passes. RFC 3261 §13 (INVITE transactions) and §20 (headers) have many edge cases. Treat SIP as a protocol that needs integration testing, not just unit testing.
2. **CallManager is the right abstraction boundary**: Keep audio bridge orchestration in CallManager, keep SIP parsing in userAgent/sip modules, keep RTP in rtpHandler. This separation made Phase 3 changes cleanly additive.
3. **Human verification gap is inherent**: 11 items require live SIP credentials. For SIP/RTP services, plan for a separate live-network test phase or document this gap clearly. Automated tests can only go so far.
4. **pnpm fetch pattern pays off immediately**: Add it to the Dockerfile template for all Node.js projects.

### Cost Observations

- Model mix: claude-sonnet-4-6 throughout
- Sessions: ~1 day, single milestone
- Notable: High iteration velocity — 10 plans in one day, including debugging SIP protocol edge cases

---

## Cross-Milestone Trends

### Process Evolution

| Milestone | Phases | Plans | Key Change |
|-----------|--------|-------|------------|
| v1.0 MVP | 3 | 10 | Initial project — established all patterns |

### Cumulative Quality

| Milestone | Requirements | Coverage | Notes |
|-----------|-------------|----------|-------|
| v1.0 | 27/27 | Automated + 11 human-needed | FD leak: verified |

### Top Lessons (Verified Across Milestones)

1. SIP RFC compliance requires live testing — automated checks can verify wiring but not protocol correctness
2. Per-call factory pattern (createRtpHandler, createWsClient) makes concurrent isolation trivially correct
