# Milestones

## v1.0 MVP (Shipped: 2026-03-03)

**Phases completed:** 3 phases (Foundation · Core Bridge · Resilience), 10 plans
**Files:** 39 files, 6,626 LOC (1,648 TypeScript src)
**Git range:** `feat(01-01)` → `feat(03-01)`

**Delivered:** Incoming SIP calls from sipgate trunking are bridged bidirectionally to a WebSocket endpoint using the Twilio Media Streams protocol, with resilient reconnection and zero file-descriptor leaks.

**Key accomplishments:**
1. TypeScript/ESM scaffold with Zod 4 fail-fast config validation and pino structured JSON logging
2. SIP.js registration against sipgate trunking with automatic re-registration (refreshFrequency 90%)
3. Multi-stage Docker image on node:22-alpine with pnpm fetch layer caching and `network_mode: host`
4. Bidirectional RTP↔WebSocket bridge implementing full Twilio Media Streams protocol (connected/start/media/stop/dtmf events)
5. CallManager orchestrating concurrent SIP INVITE lifecycles with independent per-call sessions
6. WebSocket reconnect loop (exponential backoff 1s/2s/4s, 30s budget) with RTP silence injection; zero FD leak verified over 20 sequential calls

**Known Gaps (tech debt — no blockers):**
- M-01: `src/logger/index.ts:4` reads `LOG_LEVEL` directly from `process.env` instead of validated `config.LOG_LEVEL`
- M-02: `callManager.ts:461` DTMF handler lacks explicit `wsReconnecting` gate (safe but inconsistent)
- 11 human verification items require live sipgate credentials (live SIP registration, audio E2E, concurrent calls)

---

