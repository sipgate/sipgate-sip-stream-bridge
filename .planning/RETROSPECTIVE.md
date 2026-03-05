# Project Retrospective: audio-dock

*A living document updated after each milestone. Lessons feed forward into future planning.*

---

## Milestone: v2.0 — Go Rewrite

**Shipped:** 2026-03-05
**Phases:** 5 (4–8) | **Plans:** 9 | **Timeline:** 2 days (2026-03-03 → 2026-03-04)

### What Was Built

- Go module scaffold with zerolog JSON logging and fail-fast env config validation via go-simpler/env — zero-allocation hot path
- ~1.06 MB Docker image via `FROM scratch` static binary with `CGO_ENABLED=0` GOARCH=amd64 cross-compile
- sipgo v1.2.0 SIP registration with Digest Auth challenge/response and automatic re-registration at 75% of server-granted Expires
- Full Twilio Media Streams bidirectional RTP↔WebSocket bridge: connected/start/media/stop events, per-call goroutine lifecycle
- sync.Map CallManager + channel-based PortPool (O(1) lock-free acquire/release) for concurrent call isolation
- WebSocket reconnect loop (1s/2s/4s exponential backoff, 30s budget) + RFC 4733 DTMF End-bit deduplication by RTP timestamp
- Graceful SIGTERM drain: BYE all active calls (8s budget) → UNREGISTER (5s budget) → exit
- `/health` JSON endpoint + `/metrics` Prometheus exposition with 5 custom metrics on isolated registry

### What Worked

- **Phase-ordered dependency structure**: Go Scaffold → SIP Registration → Bridge → Resilience → Observability meant each phase left a clean, tested API surface. No backtracking.
- **TDD for pure Go logic**: PortPool, SDP parsing, parseTelephoneEvent all had RED-first test suites — caught edge cases before integration
- **wsSignal pattern**: sync.Once-guarded channel for multi-goroutine failure signaling prevented double-close panics without a mutex
- **per-connection wsWg separate from session s.wg**: allowed independent WS goroutine drain on reconnect without touching the persistent RTP goroutine lifecycle
- **Custom prometheus.Registry**: keeping Go runtime metrics out of /metrics kept the scrape output clean and matching OBS-03 literally

### What Was Inefficient

- **4 SIP runtime bugs during Phase 6 live testing**: sipgo API differences from documentation (WithUserAgentName vs WithUserAgent), Record-Route handling, 200 OK synchronous send, CANCEL handling — required multiple fix commits post-implementation
- **WSB-01..06 checkboxes not updated** during Phase 6 completion — only discovered at milestone archive time. Should update REQUIREMENTS.md checkboxes as part of each plan's docs commit.
- **SDP_CONTACT_IP initially required** then changed to optional with auto-detect during a fix commit — plan should have specified optionality upfront
- **gsd-tools milestone complete created stub entry** (0 tasks, no accomplishments) — the tool doesn't auto-extract SUMMARY.md content; manual enrichment needed every time

### Patterns Established

- **nil-safe metrics guard**: `if m.metrics != nil { ... }` pattern in all hot paths enables tests to pass nil without a real registry
- **slog-zerolog bridge**: `samber/slog-zerolog` funnels sipgo's slog output into zerolog JSON stream — avoids split log formats
- **DTMF PT dynamic extraction**: always extract `telephone-event` PT from SDP offer via pion/sdp; never hardcode 101 (sipgate uses 113)
- **Drain budget split**: 8s for calls + 5s for unregister; configure SIGTERM kill timeout ≥15s in prod
- **gobwas/ws write safety**: `wg.Wait()` before `sendStop` — rtpToWS is the sole writer during audio; sendStop only after drain

### Key Lessons

1. **sipgo v1.2.0 API surface differs from docs**: treat SIP library methods as requiring live verification pass, not just unit tests. `WithUserAgent` vs `WithUserAgentName`, `Server.Close()` returning nil, `ClientRequestRegisterBuild` option — these required live corrections.
2. **RFC 4733 End-bit deduplication is essential**: sipgate sends 3–5 retransmits per DTMF keypress (all with same RTP timestamp). Without deduplication, each keypress fires 3–5 events. Filter on `End=1 && timestamp not seen`.
3. **sync.Map polling is correct for drain**: avoid adding WaitGroup to the session close path (creates coupling); instead poll `ActiveCount()` with a 50ms tick + context deadline. Clean, bounded, no hot-path impact.
4. **REQUIREMENTS.md checkbox discipline**: mark requirements checked at plan-completion time, not milestone-completion time. Stale checkboxes look like gaps.

### Cost Observations

- Model mix: claude-sonnet-4-6 throughout
- Sessions: 2 days, 9 plans
- Notable: Go rewrite with full feature parity in 2 days including SIP protocol debugging

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
| v2.0 Go Rewrite | 5 | 9 | Language rewrite — same external interface, faster execution per plan |

### Cumulative Quality

| Milestone | Requirements | Coverage | Notes |
|-----------|-------------|----------|-------|
| v1.0 | 27/27 | Automated + 11 human-needed | FD leak: verified |
| v2.0 | 29/29 | Automated + human-needed | WSB-01..06 docs gap; all implemented |

### Top Lessons (Verified Across Milestones)

1. SIP RFC compliance requires live testing — automated checks can verify wiring but not protocol correctness
2. Per-call factory pattern (createRtpHandler / CallSession) makes concurrent isolation trivially correct
3. Drop RTP during reconnect (not buffer) — bounded memory, acceptable audio gap, simpler code
4. Update REQUIREMENTS.md checkboxes at plan-completion time, not milestone-completion time
