# audio-dock

## What This Is

A Node.js/TypeScript service that registers with sipgate trunking via SIP, accepts incoming calls, and bridges the audio bidirectionally to a configurable WebSocket endpoint. The WebSocket protocol follows the Twilio Media Streams format, making it drop-in compatible with AI voice-bot backends. Runs as a Docker container with resilient WebSocket reconnection and graceful shutdown.

## Core Value

Incoming SIP calls from sipgate trunking are reliably bridged to a WebSocket endpoint in real-time — audio flows both ways, the connection stays alive through transient drops, and the integration is drop-in compatible with Twilio Media Streams consumers.

## Current Milestone: v2.0 Go Rewrite

**Goal:** Rewrite audio-dock in Go to eliminate GC-induced audio jitter and achieve deterministic sub-millisecond latency — same external interface, smaller Docker image, observability included.

**Target features:**
- Complete Go implementation: SIP registration, bidirectional RTP↔WebSocket bridge, Twilio Media Streams protocol
- Deterministic audio: no drain-queue workarounds; Go goroutines + raw `net.UDPConn` handle UDP natively
- `/health` + `/metrics` (Prometheus) via stdlib `net/http` — no additional framework needed
- Static Go binary in Docker FROM scratch or distroless — significantly smaller image than Node.js runtime

## Requirements

### Validated

- ✓ Service registers with sipgate trunking via SIP on startup — v1.0 (SIP.js 0.21.x + ws polyfill, refreshFrequency 90)
- ✓ Service accepts incoming SIP calls and negotiates PCMU codec — v1.0 (full INVITE/ACK/BYE/OPTIONS handling)
- ✓ Each call establishes a dedicated WebSocket connection to the configured target URL — v1.0 (per-call WsClient factory)
- ✓ Audio from caller is forwarded to WebSocket in Twilio Media Streams format (base64 mulaw 8kHz) — v1.0
- ✓ Audio received from WebSocket is played back to the caller (bidirectional) — v1.0
- ✓ Multiple concurrent calls supported, each with independent WebSocket connection — v1.0 (CallSession Map)
- ✓ If WebSocket is unavailable at call start, the SIP call is rejected with 503 — v1.0
- ✓ If WebSocket drops during an active call, service reconnects and keeps call alive — v1.0 (exponential backoff 1s/2s/4s, 30s budget)
- ✓ All credentials and target URL configured via environment variables — v1.0 (Zod 4 validated config)
- ✓ Service runs in Docker with multi-stage build on node:22-alpine — v1.0

### Active

- [ ] Complete Go rewrite with 1:1 feature parity to v1.0 (SIP bridge + WS bridge + resilience)
- [ ] `GET /health` returns JSON with registration status and active call count (OBS-02)
- [ ] `GET /metrics` returns Prometheus exposition format with key counters (OBS-03)
- [ ] Static Go binary Docker image — no Node.js runtime

### Out of Scope

- Outbound call initiation — inbound only (different state machine, v2 candidate)
- Web UI or management dashboard — config is env-only
- Call recording or storage — audio is streamed, not persisted
- Multiple target URLs / routing logic — single fixed WebSocket URL (dumb pipe by design)
- SRTP / media encryption — plain RTP assumed for internal infrastructure use
- Multi-codec support (G.722, Opus) — PCMU only; transcoding belongs in WS consumer
- Outbound calls — v2 candidate

## Context

**Shipped v1.0** with 1,648 LOC TypeScript, 39 files.

**Tech stack:** Node.js 22, TypeScript 5.9 (NodeNext ESM), SIP.js 0.21.2, pino 10.3.1, ws 8.19.0, Zod 4.3.6, tsup, tsx, Docker node:22-alpine.

**Known issues / tech debt:**
- `src/logger/index.ts:4` reads `LOG_LEVEL` directly from `process.env` instead of validated `config.LOG_LEVEL` (M-01)
- `callManager.ts:461` DTMF handler lacks `wsReconnecting` gate (cosmetic, functionally safe) (M-02)
- 11 human verification items require live sipgate credentials and network (E2E test gap)

**Next milestone focus:** Phase 4 Observability — HTTP health + Prometheus metrics endpoints.

## Constraints

- **Tech Stack**: Node.js + TypeScript — no compiled sidecar processes
- **Language**: Go — deterministic GC, goroutines, direct syscall UDP access
- **SIP Library**: TBD (research needed — sipgo/emiago is the main candidate)
- **Transport**: UDP/TCP SIP, RTP audio
- **Config**: Environment variables only — no config files, suitable for Docker/K8s secrets
- **Backwards compat**: Same env var names as v1.0, same WS protocol — drop-in replacement

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| SIP.js over drachtio | No C++ sidecar, simpler Docker setup | ✓ Good — works in Node.js with ws polyfill |
| Twilio Media Streams format | Drop-in compatibility with existing AI voice backends | ✓ Good — validated full protocol (connected/start/media/stop/dtmf) |
| Reject call if WS unavailable | Fail fast is cleaner than silently dropping audio | ✓ Good — 503 rejection implemented |
| Reconnect on WS drop, hold call | Better UX than forcing caller to redial on transient WS issues | ✓ Good — backoff loop + silence injection |
| network_mode: host for Docker | RTP requires no NAT; Docker port-proxy adds ~10ms UDP jitter | ✓ Good — essential for RTP timing |
| Zod 4 for config validation | Single-source-of-truth env var schema with fail-fast | ✓ Good — note: logger bypasses this for LOG_LEVEL (M-01) |
| Drop RTP during WS reconnect (not buffer) | Bounded memory; audio gap is brief and acceptable | ✓ Good — consistent with protocol design |
| pnpm fetch layer in Dockerfile | Cache layer invalidates only on lockfile change, not source | ✓ Good — fast rebuilds |

---
*Last updated: 2026-03-03 after v1.0 milestone; v2.0 Go Rewrite started*
