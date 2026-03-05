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

## Milestone: v2.1 — Twilio Media Streams: Complete Protocol

**Shipped:** 2026-03-05
**Phases:** 3 (9–11) | **Plans:** 8 | **Timeline:** 1 day (2026-03-05)

### What Was Built

- Go `outboundFrame` tagged union replacing bare `chan []byte` packetQueue — mark/audio discrimination at consumer
- Mark/clear barge-in protocol across wsToRTP, rtpPacer, wsPacer: sentinel ordering, immediate fast-path, drain-on-clear
- `mark_echoed_total`, `clear_received_total`, `sip_options_failures_total` Prometheus counters
- Go `optionsKeepaliveLoop` with 2-failure threshold state machine + `sync.Mutex` on all `doRegister` call sites
- Pure helper functions (`applyOptionsResponse`, `isOptionsFailure`, `isOptionsAuth`) enabling table-driven unit tests without a live SIP server
- Vitest 4.0.18 installed in Node.js reference implementation; 12/12 tests passing
- Node.js `DrainItem` tagged-union queue + extended `WsClient` interface (`onMark`/`sendMark`/`sendClear`)
- Node.js `applyOptionsResponse` exported pure function with CSeq routing to avoid OPTIONS corrupting REGISTER state machine

### What Worked

- **Incremental scaffold-then-wire pattern**: Plan 01 adds types/fields (always compiles), Plan 02 adds behavior — codebase never breaks between commits. Zero build failures across all 8 plans.
- **Pure state machine functions**: extracting `applyOptionsResponse` (both Go and Node.js) as side-effect-free functions enabled exhaustive table-driven tests without goroutines or UDP sockets. Very fast, very clean.
- **Channel-logic tests without goroutines**: MARK-01 through MARK-04 verified by directly simulating channel operations inline. Race-detector-clean by construction. Mirrors the DTMFDeduplication pattern already established in v2.0.
- **Go behavioral reference for Node.js**: implementing Go first then using it as a spec for Node.js meant no ambiguity in Node.js requirements. Behavioral parity was explicit and verifiable.
- **Sole-writer invariant (wsPacer)**: locked decision before Phase 9 started prevented any temptation to write mark echoes from multiple goroutines. No WS write races.

### What Was Inefficient

- **VALIDATION.md frontmatter never updated from draft**: all 3 phases have `nyquist_compliant: false` / `wave_0_complete: false` because the draft frontmatter was never updated after execution. Should update immediately after each plan's test run confirms green.
- **gsd-tools milestone complete created stub entry**: same issue as v2.0 — accomplishments show "(none recorded)" because the tool can't extract `provides` fields from SUMMARY.md. Manual enrichment still required.
- **SUMMARY.md `provides` vs `requirements_completed` key**: using a non-standard frontmatter key means cross-reference tools return N/A for all plans. Should use `requirements_completed` going forward.

### Patterns Established

- **Increment-always pattern**: type/counter scaffolding in Plan N, behavioral wiring in Plan N+1 — codebase always compiles between plan commits
- **Tagged-union channel entry**: `outboundFrame{audio []byte, mark string}` in Go / `DrainItem = Buffer | {markName: string}` in Node.js — discriminate at consumer with nil/isBuffer check
- **CSeq response routing (Node.js)**: dispatch SIP responses by method in CSeq header before state-machine handling — prevents OPTIONS/REGISTER response cross-contamination
- **Dual-timer keepalive**: `setInterval` for periodic send + per-send `setTimeout` for timeout detection — avoids timer leak if interval fires before previous ping resolves

### Key Lessons

1. **Lock concurrency decisions before implementation starts**: the sole-writer invariant and doRegister mutex were locked decisions in CONTEXT.md before a line of implementation was written. This prevented mid-implementation re-architecture.
2. **Pure function extraction is worth the extra line**: `applyOptionsResponse` as a pure function in both Go and Node.js gave free exhaustive test coverage without any mock infrastructure. Always extract state machine logic to pure functions.
3. **Channel-logic tests scale well**: synchronous channel simulation (no goroutines) tests concurrent correctness cheaply. For any goroutine protocol with channels, consider this pattern first before reaching for goroutine harnesses.
4. **VALIDATION.md frontmatter should be a checklist item in every plan's docs commit**: not a separate step after the fact.

### Cost Observations

- Model mix: claude-sonnet-4-6 throughout
- Sessions: 1 day, 8 plans (avg ~10 min/plan)
- Notable: Go + Node.js dual-language implementation in parallel with full test coverage in a single day

---

## Cross-Milestone Trends

### Process Evolution

| Milestone | Phases | Plans | Key Change |
|-----------|--------|-------|------------|
| v1.0 MVP | 3 | 10 | Initial project — established all patterns |
| v2.0 Go Rewrite | 5 | 9 | Language rewrite — same external interface, faster execution per plan |
| v2.1 Protocol | 3 | 8 | Dual-language feature addition — pure function pattern, channel-logic tests |

### Cumulative Quality

| Milestone | Requirements | Coverage | Notes |
|-----------|-------------|----------|-------|
| v1.0 | 27/27 | Automated + 11 human-needed | FD leak: verified |
| v2.0 | 29/29 | Automated + human-needed | WSB-01..06 docs gap; all implemented |
| v2.1 | 15/15 | Automated + human-needed | VALIDATION.md frontmatter gap (cosmetic) |

### Top Lessons (Verified Across Milestones)

1. SIP RFC compliance requires live testing — automated checks can verify wiring but not protocol correctness
2. Per-call factory pattern (createRtpHandler / CallSession) makes concurrent isolation trivially correct
3. Drop RTP during reconnect (not buffer) — bounded memory, acceptable audio gap, simpler code
4. Update REQUIREMENTS.md checkboxes at plan-completion time, not milestone-completion time
5. Lock concurrency decisions (sole-writer invariants, mutexes) in CONTEXT.md before implementation begins
6. Pure state machine functions (applyOptionsResponse pattern) give free table-driven test coverage — always extract
