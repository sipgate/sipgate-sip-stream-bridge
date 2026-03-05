# Milestones

## v2.0 Go Rewrite (Shipped: 2026-03-05)

**Phases completed:** 5 phases (Go Scaffold · SIP Registration · Inbound Call + RTP Bridge · WebSocket Resilience + DTMF · Lifecycle + Observability), 9 plans
**Files:** 15 Go source files, ~2,900 LOC Go
**Git range:** `feat(04-01)` → `feat(08-02)`
**Timeline:** 2026-03-03 → 2026-03-04 (2 days)

**Delivered:** Complete Go rewrite of audio-dock — same external interface as v1.0, deterministic audio with goroutine-based RTP, ~1 MB Docker image from scratch, Prometheus observability included.

**Key accomplishments:**
1. Go module scaffold with zerolog JSON logging and fail-fast env config validation via go-simpler/env — zero-allocation hot path, matches all v1.0 env var names for drop-in compatibility
2. ~1.06 MB Docker image via `FROM scratch` static binary (vs 180 MB Node.js Alpine baseline) with `CGO_ENABLED=0` cross-compile for amd64
3. sipgo v1.2.0 SIP registration with Digest Auth challenge/response and automatic re-registration at 75% of server-granted Expires interval
4. Full Twilio Media Streams bidirectional RTP↔WebSocket bridge: connected/start/media/stop events, per-call goroutine lifecycle, concurrent calls via sync.Map + channel-based PortPool
5. WebSocket reconnect loop (1s/2s/4s exponential backoff, 30s budget) + RFC 4733 DTMF End-bit deduplication by RTP timestamp (sipgate PT 113)
6. Graceful SIGTERM drain (BYE all active calls → UNREGISTER → exit within 10s) + `/health` + `/metrics` Prometheus endpoints (5 metrics on custom registry)

**Known Gaps:**
- WSB-01..06 checkboxes were not updated in REQUIREMENTS.md during Phase 6 (documentation gap, all implemented in 06-03)
- Human verification items require live sipgate credentials (live SIP registration, audio E2E, concurrent calls)
- DTMF PT mismatch monitoring: sipgate uses PT 113, service extracts dynamically — verify on first live registration

---

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
